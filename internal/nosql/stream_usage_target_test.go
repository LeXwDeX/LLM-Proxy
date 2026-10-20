package nosql

import (
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

func TestStreamUsageTargetPersistence(t *testing.T) {
	db := testDB(t)
	store := NewTargetStore(db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTargets)).Put([]byte("old"), []byte(`{"name":"old","endpoint_type":"openai","endpoint":"https://fixture.invalid","usage_adapter":"standard","api_key":"fixture-key"}`))
	}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].RequestStreamUsageIfMissing {
		t.Fatalf("old record changed: %+v", list)
	}
	for _, enabled := range []bool{true, false} {
		if err := store.Update("old", config.Target{Name: "old", EndpointType: "openai", Endpoint: "https://fixture.invalid", APIKey: "fixture-key", UsageAdapter: "standard", RequestStreamUsageIfMissing: enabled}); err != nil {
			t.Fatal(err)
		}
		list, err = store.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].RequestStreamUsageIfMissing != enabled {
			t.Fatalf("round-trip: %+v", list)
		}
	}
}
