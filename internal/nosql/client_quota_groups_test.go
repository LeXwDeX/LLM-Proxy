package nosql

import (
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func seedModelGroups(t *testing.T, db *bolt.DB, names ...string) {
	t.Helper()
	store := NewModelGroupStore(db)
	for _, name := range names {
		if err := store.Create(config.ModelGroup{Name: name, CostCoefficient: 1}); err != nil {
			t.Fatalf("seed model group %q: %v", name, err)
		}
	}
}

// coefPtr 返回个人系数指针（nil=默认1，0=不限，>0=乘数）。
func coefPtr(v float64) *float64 { return &v }

func TestClientQuotaStoreReplaceGetDelete(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium")

	groups := []config.QuotaGroup{{Name: "premium", Coefficient: coefPtr(0.8)}}
	if err := store.Replace("alice", groups); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := store.Get("ALICE")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, groups) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	if err := store.Replace("alice", nil); err != nil {
		t.Fatalf("Replace empty: %v", err)
	}
	got, err = store.Get("alice")
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected cleared entry, got %+v", got)
	}

	if err := store.Replace("alice", groups); err != nil {
		t.Fatalf("Replace again: %v", err)
	}
	if err := store.Delete("Alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = store.Get("alice")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected deleted entry, got %+v", got)
	}
}

func TestClientQuotaStoreListPreservesClientName(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium", "basic")

	if err := store.Replace("Alice", []config.QuotaGroup{{Name: "premium"}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := store.Replace("bob", []config.QuotaGroup{{Name: "basic"}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	all, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if groups, ok := all["Alice"]; !ok || len(groups) != 1 || groups[0].Name != "premium" {
		t.Fatalf("Alice entry missing or wrong: %+v", all)
	}
	if _, ok := all["bob"]; !ok {
		t.Fatalf("bob entry missing: %+v", all)
	}
}

func TestClientQuotaStoreReplaceMany(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium")

	groups := []config.QuotaGroup{{Name: "premium", Coefficient: coefPtr(2)}}
	updated, err := store.ReplaceMany([]string{"alice", "bob"}, groups)
	if err != nil {
		t.Fatalf("ReplaceMany: %v", err)
	}
	if !reflect.DeepEqual(updated, []string{"alice", "bob"}) {
		t.Fatalf("unexpected updated names: %v", updated)
	}
	for _, name := range []string{"alice", "bob"} {
		got, err := store.Get(name)
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		if !reflect.DeepEqual(got, groups) {
			t.Fatalf("groups mismatch for %s: %+v", name, got)
		}
	}

	if _, err := store.ReplaceMany([]string{"alice", "alice"}, groups); err == nil {
		t.Fatal("expected duplicate client to be rejected")
	}

	updated, err = store.ReplaceMany([]string{"alice"}, nil)
	if err != nil {
		t.Fatalf("ReplaceMany clear: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("unexpected updated names: %v", updated)
	}
	got, err := store.Get("alice")
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected cleared entry, got %+v", got)
	}
}

func TestClientQuotaStoreRejectsUnknownModelGroup(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	if err := store.Replace("alice", []config.QuotaGroup{{Name: "missing"}}); err == nil {
		t.Fatal("expected unknown model group to be rejected")
	}
	// company_general 豁免全局模型组存在性检查。
	if err := store.Replace("alice", []config.QuotaGroup{{Name: "company_general"}}); err != nil {
		t.Fatalf("company_general should be accepted: %v", err)
	}
}

func TestClientQuotaStoreRejectsInvalidGroups(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)

	cases := []struct {
		name   string
		groups []config.QuotaGroup
	}{
		{name: "empty group name", groups: []config.QuotaGroup{{Name: ""}}},
		{name: "duplicate group name", groups: []config.QuotaGroup{{Name: "a"}, {Name: "A"}}},
		{name: "negative coefficient", groups: []config.QuotaGroup{{Name: "a", Coefficient: coefPtr(-1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.Replace("alice", tc.groups); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestClientQuotaStoreArchiveAndPurge(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium")

	groups := []config.QuotaGroup{{Name: "premium", Coefficient: coefPtr(0.8)}}
	if err := store.Replace("Alice", groups); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if err := store.Archive(config.Client{Name: "Alice", IPAddress: "192.0.2.9", JobLevel: "L5", ProjectGroup: "AI"}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// List skips archived entries.
	active, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected archived entry to be skipped, got %+v", active)
	}

	// ListEntries includes the archived snapshot with the original config.
	entries, err := store.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	entry, ok := entries["Alice"]
	if !ok {
		t.Fatalf("archived entry missing: %+v", entries)
	}
	if entry.DeletedAt == "" || entry.IPAddress != "192.0.2.9" || entry.JobLevel != "L5" || entry.ProjectGroup != "AI" {
		t.Fatalf("archive snapshot wrong: %+v", entry)
	}
	if len(entry.Groups) != 1 || entry.Groups[0].Name != "premium" {
		t.Fatalf("archived groups wrong: %+v", entry.Groups)
	}

	// Archive without an existing entry now writes an empty-group snapshot.
	if err := store.Archive(config.Client{Name: "nobody"}); err != nil {
		t.Fatalf("Archive no-entry: %v", err)
	}
	entries, err = store.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries after no-entry archive: %v", err)
	}
	nobody, ok := entries["nobody"]
	if !ok {
		t.Fatalf("no-entry archive should create a snapshot, got %+v", entries)
	}
	if len(nobody.Groups) != 0 || nobody.DeletedAt == "" {
		t.Fatalf("no-entry archive snapshot wrong: %+v", nobody)
	}

	// Purge removes the archived entries.
	if err := store.Delete("Alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete("nobody"); err != nil {
		t.Fatalf("Delete nobody: %v", err)
	}
	entries, err = store.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries after purge: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected purged entry to be gone, got %+v", entries)
	}
}

func TestClientQuotaStoreGetIgnoresArchivedEntry(t *testing.T) {
	db := testDB(t)
	store := NewClientQuotaStore(db)
	seedModelGroups(t, db, "premium")
	if err := store.Replace("Alice", []config.QuotaGroup{{Name: "premium", Coefficient: coefPtr(0.8)}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := store.Archive(config.Client{Name: "Alice"}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, err := store.Get("Alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected archived entry to be ignored, got %+v", got)
	}
}

func TestRenameRejectedWhenTargetNameArchived(t *testing.T) {
	db := testDB(t)
	clients := NewClientStore(db)
	quotaStore := NewClientQuotaStore(db)

	if err := clients.Create(config.Client{Name: "alice", AccessKey: "key-a", IPAddress: "192.0.2.1"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := clients.Create(config.Client{Name: "bob", AccessKey: "key-b", IPAddress: "192.0.2.2"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	seedModelGroups(t, db, "g")
	if err := quotaStore.Replace("bob", []config.QuotaGroup{{Name: "g", Coefficient: coefPtr(0.5)}}); err != nil {
		t.Fatalf("seed bob groups: %v", err)
	}
	bobSnapshot, err := clients.Get("bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if err := quotaStore.DeleteClientAndArchive(*bobSnapshot); err != nil {
		t.Fatalf("archive bob: %v", err)
	}

	// alice → bob must be rejected: bob's archived record owns the name.
	err = clients.Update("alice", config.Client{Name: "bob", AccessKey: "key-a"})
	if err == nil {
		t.Fatal("expected rename onto archived name to be rejected")
	}
	if !strings.Contains(err.Error(), "archived quota record") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The failed rename must be atomic: alice still exists, bob's archive intact.
	alice, err := clients.Get("alice")
	if err != nil || alice == nil {
		t.Fatalf("alice must survive the rejected rename: %v", err)
	}
	entries, err := quotaStore.ListEntries()
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	archived, ok := entries["bob"]
	if !ok || archived.DeletedAt == "" || len(archived.Groups) != 1 {
		t.Fatalf("bob's archive must be preserved, got %+v (ok=%v)", archived, ok)
	}

	// Clearing the archived record frees the name and the rename succeeds.
	if err := quotaStore.Delete("bob"); err != nil {
		t.Fatalf("clear archived entry: %v", err)
	}
	if err := clients.Update("alice", config.Client{Name: "bob", AccessKey: "key-a"}); err != nil {
		t.Fatalf("rename after clearing archive: %v", err)
	}
}
