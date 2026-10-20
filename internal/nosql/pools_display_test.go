package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

func TestPoolDisplayPersistenceAndLegacyRecords(t *testing.T) {
	db := testDB(t)
	store := NewPoolStore(db)
	if err := store.Create(config.DeploymentPool{Name: "OpenAI-PLAN"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(config.DeploymentPool{Name: "OPENAI-plan"}); err == nil {
		t.Fatal("case-insensitive uniqueness lost")
	}
	// Read through another store to prove this is persisted, not an in-memory label.
	pools, err := NewPoolStore(db).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Name != "openai-plan" || pools[0].DisplayName != "OpenAI-PLAN" {
		t.Fatalf("created pools: %+v", pools)
	}
	pool := pools[0]
	pool.DisplayName = "OPENAI-Plan"
	if err := store.Update("openai-plan", pool); err != nil {
		t.Fatal(err)
	}
	pool.DisplayName = "different"
	if err := store.Update("openai-plan", pool); err == nil {
		t.Fatal("display name cannot change identity")
	}
	// Old persisted records have no display_name field and must remain usable.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketPools)).Put([]byte("legacy"), []byte(`{"name":"legacy","paused":true}`))
	}); err != nil {
		t.Fatal(err)
	}
	pools, err = NewPoolStore(db).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 || pools[0].Name != "openai-plan" || pools[0].DisplayName != "OPENAI-Plan" || pools[1].DisplayName != "legacy" || !pools[1].Paused {
		t.Fatalf("updated and legacy pools: %+v", pools)
	}
	if err := store.Delete("OPENAI-PLAN"); err != nil {
		t.Fatal(err)
	}
}
