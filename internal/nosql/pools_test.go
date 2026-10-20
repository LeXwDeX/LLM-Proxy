package nosql

import (
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestPoolStoreCRUDPreservesOrder(t *testing.T) {
	db := testDB(t)
	store := NewPoolStore(db)

	first := config.DeploymentPool{Name: "alpha", Description: "first pool"}
	second := config.DeploymentPool{Name: "beta"}

	if err := store.Create(first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := store.Create(second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	if err := store.Create(second); err == nil {
		t.Fatalf("expected duplicate pool error")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected duplicate error: %v", err)
	}

	pools, err := store.List()
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 2 || pools[0].Name != "alpha" || pools[1].Name != "beta" {
		t.Fatalf("unexpected pool order: %+v", pools)
	}
	if pools[0].Description != "first pool" {
		t.Fatalf("unexpected description: %+v", pools[0])
	}

	second.Description = "updated"
	if err := store.Update("beta", second); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if err := store.Delete("alpha"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}

	pools, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "beta" || pools[0].Description != "updated" {
		t.Fatalf("unexpected pools after update/delete: %+v", pools)
	}
}

func TestPoolStoreUpdateRejectsRename(t *testing.T) {
	db := testDB(t)
	store := NewPoolStore(db)

	if err := store.Create(config.DeploymentPool{Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := store.Update("alpha", config.DeploymentPool{Name: "renamed"})
	if err == nil {
		t.Fatalf("expected rename to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("unexpected rename error: %v", err)
	}

	if err := store.Update("missing", config.DeploymentPool{Name: "missing"}); err == nil {
		t.Fatalf("expected not-found error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected not-found error: %v", err)
	}

	pools, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "alpha" {
		t.Fatalf("rename attempt must not mutate stored pools: %+v", pools)
	}
}

func TestPoolStorePauseRoundTrip(t *testing.T) {
	db := testDB(t)
	store := NewPoolStore(db)

	if err := store.Create(config.DeploymentPool{Name: "alpha"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	paused := config.DeploymentPool{Name: "alpha", Paused: true}
	if err := store.Update("alpha", paused); err != nil {
		t.Fatalf("update paused: %v", err)
	}
	pools, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 || !pools[0].Paused {
		t.Fatalf("expected paused=true to persist: %+v", pools)
	}
}

func TestMigratePoolsFromConfigOnlyWhenEmpty(t *testing.T) {
	db := testDB(t)
	legacy := []config.DeploymentPool{
		{Name: "first", Description: "one"},
		{Name: "second"},
	}

	migrated, err := MigratePoolsFromConfig(db, legacy)
	if err != nil {
		t.Fatalf("migrate pools: %v", err)
	}
	if !migrated {
		t.Fatalf("expected migration to copy pools")
	}

	pools, err := NewPoolStore(db).List()
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 2 || pools[0].Name != "first" || pools[1].Name != "second" {
		t.Fatalf("unexpected migrated pools: %+v", pools)
	}

	migrated, err = MigratePoolsFromConfig(db, []config.DeploymentPool{{Name: "third"}})
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if migrated {
		t.Fatalf("expected second migration to skip non-empty pool store")
	}

	pools, err = NewPoolStore(db).List()
	if err != nil {
		t.Fatalf("list after second migration: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("second migration must not mutate pool store: %+v", pools)
	}
}

func TestTargetStorePoolsViewSharesDatabase(t *testing.T) {
	db := testDB(t)
	targetStore := NewTargetStore(db)

	poolStore := targetStore.Pools()
	if poolStore == nil {
		t.Fatalf("expected non-nil pool view")
	}
	if err := poolStore.Create(config.DeploymentPool{Name: "alpha"}); err != nil {
		t.Fatalf("create via view: %v", err)
	}

	// The view must share the same underlying db as an independent PoolStore.
	pools, err := NewPoolStore(db).List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "alpha" {
		t.Fatalf("view must share database: %+v", pools)
	}
}
