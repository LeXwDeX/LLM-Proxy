package nosql

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestDirectRawIndependentScope(t *testing.T) {
	at := fixedPlainEvent().Timestamp
	for _, tc := range []struct {
		name      string
		raw       []byte
		client    string
		wantError bool
	}{
		{"known other client bad tokens", []byte(fmt.Sprintf(`{"client_name":"bob","timestamp":%q,"model":"executor","input_tokens":"bad"}`, at.Format(time.RFC3339))), "alice", false},
		{"known same client bad tokens", []byte(fmt.Sprintf(`{"client_name":"bob","timestamp":%q,"model":"executor","input_tokens":"bad"}`, at.Format(time.RFC3339))), "bob", true},
		{"other client unknown time", []byte(`{"client_name":"alice","timestamp":"bad"}`), "bob", false},
		{"same client unknown time", []byte(`{"client_name":"alice","timestamp":"bad"}`), "alice", true},
		{"unknown invalid JSON", []byte(`{broken`), "alice", true},
		{"conflicting client keys", []byte(fmt.Sprintf(`{"client_name":"alice","CLIENT_NAME":"bob","timestamp":%q,"input_tokens":"bad"}`, at.Format(time.RFC3339))), "carol", true},
		{"duplicate client keys", []byte(fmt.Sprintf(`{"client_name":"alice","client_name":"bob","timestamp":%q,"input_tokens":"bad"}`, at.Format(time.RFC3339))), "carol", true},
		{"invalid UTF8", []byte(fmt.Sprintf("{\"client_name\":\"bob\",\"timestamp\":%q,\"model\":\"\xff\"}", at.Format(time.RFC3339))), "alice", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("legacy-scope-fixture"), tc.raw)
			}); err != nil {
				t.Fatal(err)
			}
			from, to := at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour)
			units, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: tc.client, From: &from, To: &to}, replayCosts, false)
			if tc.wantError && (len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete)) {
				t.Fatalf("unsafe result: %v %v", units, err)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("disjoint scope: %v", err)
			}
		})
	}
}

func TestDirectRawOwnerInvalidSourceCannotExclude(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = "bob"
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	value := []byte(fmt.Sprintf(`{"client_name":"bob","timestamp":%q,"model":"executor","input_tokens":"bad"}`, event.Timestamp.Format(time.RFC3339)))
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, value); err != nil {
			return err
		}
		return tx.Bucket([]byte(rawOwnerBucket)).Put(key, []byte("{bad"))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
	if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("invalid source excluded: %v %v", units, err)
	}
}

func TestDirectRawOwnerScopeConflictFailsClosed(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = "bob"
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	value := []byte(fmt.Sprintf(`{"client_name":"alice","timestamp":%q,"model":"executor","input_tokens":"bad"}`, event.Timestamp.Format(time.RFC3339)))
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(key, value) }); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "carol", From: &from, To: &to}, replayCosts, false)
	if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("owner conflict excluded: %v %v", units, err)
	}
}
