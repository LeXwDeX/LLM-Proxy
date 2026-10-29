package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

func readTxID(t *testing.T, db *bolt.DB) int {
	t.Helper()
	var id int
	if err := db.View(func(tx *bolt.Tx) error {
		id = tx.ID()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// Admin reads call these migrations on every request. With nothing to migrate
// they must not queue behind bbolt's single writer or fsync a no-op commit.
func TestMigrationsWithNothingToDoDoNotWrite(t *testing.T) {
	db := testDB(t)
	legacy := []config.Target{
		{Name: "empty", Endpoint: "https://dashscope.aliyuncs.com", APIKey: "test"},
		{Name: "auto", Endpoint: "https://api.z.ai", APIKey: "test", UsageAdapter: "auto"},
	}
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	store := NewTargetStore(db)
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}

	before := readTxID(t, db)
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || imported {
		t.Fatalf("second import: %v %v", imported, err)
	}
	if err := MigrateUsageAdapterExplicit(db, nil); err != nil {
		t.Fatal(err)
	}
	if after := readTxID(t, db); after != before {
		t.Fatalf("no-op migrations committed write transactions: txid %d -> %d", before, after)
	}
}

// The read-only precheck must not hide real work: a legacy value that appears
// after import is still converted.
func TestMigrateUsageAdapterExplicitStillWritesWhenPending(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTargets)).Put([]byte("legacy"), []byte(`{"name":"legacy","endpoint":"https://api.z.ai","usage_adapter":"auto"}`))
	}); err != nil {
		t.Fatal(err)
	}
	before := readTxID(t, db)
	if err := MigrateUsageAdapterExplicit(db, nil); err != nil {
		t.Fatal(err)
	}
	if after := readTxID(t, db); after == before {
		t.Fatal("pending migration was skipped")
	}
	var raw []byte
	if err := db.View(func(tx *bolt.Tx) error {
		raw = append([]byte(nil), tx.Bucket([]byte(BucketTargets)).Get([]byte("legacy"))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"legacy","endpoint":"https://api.z.ai","usage_adapter":"glm"}`; string(raw) != want {
		t.Fatalf("raw = %s, want %s", raw, want)
	}
}
