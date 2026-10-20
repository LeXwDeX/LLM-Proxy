package nosql

import (
	"errors"
	"reflect"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

func syncString(v string) *string { return &v }

func TestSyncQuotaCompletePlanAndScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selectors QuotaSyncSelectors
		want      []string
	}{
		{"clients", QuotaSyncSelectors{Clients: []string{" Source ", " source ", " Target ", "target", ""}}, []string{"Target"}},
		{"project", QuotaSyncSelectors{ProjectGroup: syncString(" project ")}, []string{"Other", "Target"}},
		{"job", QuotaSyncSelectors{JobLevel: syncString(" b ")}, []string{"SameRank", "Target"}},
		{"union", QuotaSyncSelectors{Clients: []string{"target", "Target", "Source"}, ProjectGroup: syncString("project"), JobLevel: syncString("B")}, []string{"Other", "SameRank", "Target"}},
	} {
		for _, empty := range []bool{false, true} {
			name := tc.name + "/complete"
			if empty {
				name = tc.name + "/empty"
			}
			t.Run(name, func(t *testing.T) {
				db := testDB(t)
				clients := NewClientStore(db)
				store := NewClientQuotaStore(db)
				seedModelGroups(t, db, "default", "zero", "positive", "old")
				for _, c := range []config.Client{
					{Name: "Source", AccessKey: "source-local", JobLevel: "A", ProjectGroup: "source"},
					{Name: "Target", AccessKey: "target-local", JobLevel: "B", ProjectGroup: "project", IPAddress: "192.0.2.7", AllowedTargets: []string{"allowed"}, AllowedPools: []string{"pool"}, QuotaDailyUSD: 17, QuotaWeeklyUSD: 19, QuotaMonthlyUSD: 23, PoolQuotas: []config.PoolQuota{{Pool: "pool", DailyUSD: 4}}},
					{Name: "Other", AccessKey: "other-local", JobLevel: "C", ProjectGroup: "project"},
					{Name: "SameRank", AccessKey: "rank-local", JobLevel: "B", ProjectGroup: "outside"},
					{Name: "Outside", AccessKey: "outside-local", JobLevel: "C", ProjectGroup: "outside"},
					{Name: "Archived", AccessKey: "archive-local", JobLevel: "B", ProjectGroup: "project"},
				} {
					if err := clients.Create(c); err != nil {
						t.Fatal(err)
					}
					if err := store.Replace(c.Name, []config.QuotaGroup{{Name: "old", Coefficient: coefPtr(5)}}); err != nil {
						t.Fatal(err)
					}
				}
				archive, err := clients.Get("Archived")
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DeleteClientAndArchive(*archive); err != nil {
					t.Fatal(err)
				}
				groups := []config.QuotaGroup{{Name: "default"}, {Name: "zero", Coefficient: coefPtr(0)}, {Name: "positive", Coefficient: coefPtr(2.5)}}
				if empty {
					groups = nil
				}
				if err := store.Replace("Source", groups); err != nil {
					t.Fatal(err)
				}
				beforeClients, err := clients.List()
				if err != nil {
					t.Fatal(err)
				}
				beforeEntries, err := store.ListEntries()
				if err != nil {
					t.Fatal(err)
				}
				beforeCatalog, err := NewModelGroupStore(db).List()
				if err != nil {
					t.Fatal(err)
				}
				got, err := store.SyncQuota("Source", tc.selectors)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("actual canonical targets: got %v want %v", got, tc.want)
				}
				afterClients, err := clients.List()
				if err != nil || !reflect.DeepEqual(afterClients, beforeClients) {
					t.Fatalf("client identity or ACL changed: %v", err)
				}
				afterCatalog, err := NewModelGroupStore(db).List()
				if err != nil || !reflect.DeepEqual(beforeCatalog, afterCatalog) {
					t.Fatalf("global model group changed: %v", err)
				}
				wantedEntries := beforeEntries
				for _, name := range tc.want {
					if empty {
						delete(wantedEntries, name)
					} else {
						wantedEntries[name] = QuotaClientEntry{Name: name, Groups: groups}
					}
				}
				afterEntries, err := store.ListEntries()
				if err != nil || !reflect.DeepEqual(afterEntries, wantedEntries) {
					t.Fatalf("complete plan or non-target protection mismatch: got %+v want %+v err=%v", afterEntries, wantedEntries, err)
				}
			})
		}
	}
}

func TestSyncQuotaValidationRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name               string
		source             string
		selectors          QuotaSyncSelectors
		corruptPlan        string
		wantSourceNotFound bool
	}{
		{name: "unknown target", source: "Source", selectors: QuotaSyncSelectors{Clients: []string{"Target", "zz-missing"}}},
		{name: "source spelling", source: "source", selectors: QuotaSyncSelectors{Clients: []string{"Target"}}, wantSourceNotFound: true},
		{name: "source missing", source: "missing", selectors: QuotaSyncSelectors{Clients: []string{"Target"}}, wantSourceNotFound: true},
		{name: "source only", source: "Source", selectors: QuotaSyncSelectors{Clients: []string{"source", " Source "}}},
		{name: "empty selection", source: "Source"},
		{name: "empty rank", source: "Source", selectors: QuotaSyncSelectors{Clients: []string{"Target"}, JobLevel: syncString(" ")}},
		{name: "unknown model group", source: "Source", selectors: QuotaSyncSelectors{Clients: []string{"Target"}}, corruptPlan: `{"name":"Source","groups":[{"name":"missing"}]}`},
		{name: "negative coefficient", source: "Source", selectors: QuotaSyncSelectors{Clients: []string{"Target"}}, corruptPlan: `{"name":"Source","groups":[{"name":"plan","coefficient":-1}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			clients, store := NewClientStore(db), NewClientQuotaStore(db)
			seedModelGroups(t, db, "plan")
			for _, name := range []string{"Source", "Target"} {
				if err := clients.Create(config.Client{Name: name, AccessKey: name + "-local"}); err != nil {
					t.Fatal(err)
				}
				if err := store.Replace(name, []config.QuotaGroup{{Name: "plan", Coefficient: coefPtr(3)}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.corruptPlan != "" {
				// Install a legacy invalid plan directly; sync must reject it before any write.
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketClientQuotaGroups)).Put([]byte("source"), []byte(tc.corruptPlan))
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.ListEntries()
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.SyncQuota(tc.source, tc.selectors)
			if err == nil || got != nil {
				t.Fatalf("expected failed command with no updated names: got %v err=%v", got, err)
			}
			if tc.wantSourceNotFound {
				if !errors.Is(err, ErrQuotaSyncSourceNotFound) {
					t.Fatalf("source error mismatch: %v", err)
				}
			} else {
				var invalid *QuotaSyncValidationError
				if !errors.As(err, &invalid) {
					t.Fatalf("expected validation error: %v", err)
				}
			}
			after, err := store.ListEntries()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed command changed entries: %v", err)
			}
		})
	}
}

func TestSyncQuotaArchivedSourcePlanIsNotCopied(t *testing.T) {
	db := testDB(t)
	clients, store := NewClientStore(db), NewClientQuotaStore(db)
	seedModelGroups(t, db, "plan")
	for _, name := range []string{"Source", "Target"} {
		if err := clients.Create(config.Client{Name: name, AccessKey: name + "-local"}); err != nil {
			t.Fatal(err)
		}
		if err := store.Replace(name, []config.QuotaGroup{{Name: "plan", Coefficient: coefPtr(3)}}); err != nil {
			t.Fatal(err)
		}
	}
	// Existing Get semantics ignore archived plans even if an active row exists.
	if err := store.Archive(config.Client{Name: "Source"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListEntries()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncQuota("Source", QuotaSyncSelectors{Clients: []string{"Target"}}); err != nil {
		t.Fatal(err)
	}
	groups, err := store.Get("Target")
	if err != nil || len(groups) != 0 {
		t.Fatalf("archived plan copied: %+v err=%v", groups, err)
	}
	after, err := store.ListEntries()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before["Source"], after["Source"]) {
		t.Fatal("source archive was overwritten")
	}
}

func TestSyncQuotaWriteFailureRollsBack(t *testing.T) {
	db := testDB(t)
	clients, store := NewClientStore(db), NewClientQuotaStore(db)
	seedModelGroups(t, db, "plan")
	for _, name := range []string{"Source", "First", "Last"} {
		if err := clients.Create(config.Client{Name: name, AccessKey: name + "-local"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Replace("Source", []config.QuotaGroup{{Name: "plan", Coefficient: coefPtr(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("First", []config.QuotaGroup{{Name: "plan", Coefficient: coefPtr(7)}}); err != nil {
		t.Fatal(err)
	}
	// A nested bucket at the last target's key forces Put to fail after First's Put.
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.Bucket([]byte(BucketClientQuotaGroups)).CreateBucket([]byte("last"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.SyncQuota("Source", QuotaSyncSelectors{Clients: []string{"First", "Last"}})
	if err == nil || got != nil {
		t.Fatalf("expected transactional write failure: names=%v err=%v", got, err)
	}
	var invalid *QuotaSyncValidationError
	if errors.As(err, &invalid) {
		t.Fatalf("write failure reported as invalid input: %v", err)
	}
	first, err := store.Get("First")
	if err != nil || len(first) != 1 || first[0].PlanCoefficient() != 7 {
		t.Fatalf("partial plan committed: %v err=%v", first, err)
	}
}
