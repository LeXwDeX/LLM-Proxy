package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestFreshProofAfterOtherOwnerQuarantine(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, batch := range []int{1, 100} {
			t.Run(fmt.Sprintf("tracked_%v_batch_%d", tracked, batch), func(t *testing.T) {
				db := testDB(t)
				store := clockedStore(db)
				bob := fixedPlainEvent()
				bob.ClientName = "bob"
				if err := store.Record(bob); err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					if !tracked {
						if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
							return err
						}
					}
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(bob.Timestamp, bob.RecordID)), []byte("{broken"))
				}); err != nil {
					t.Fatal(err)
				}
				if n, err := store.PruneEventsBefore(bob.Timestamp.Add(time.Minute), batch); err != nil || n != 1 {
					t.Fatalf("bob prune: %d %v", n, err)
				}
				alice := fixedPlainEvent()
				alice.Timestamp = bob.Timestamp.Add(2 * time.Hour)
				alice.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
				second := alice
				second.Timestamp = second.Timestamp.Add(time.Minute)
				second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"
				for _, e := range []usage.Event{alice, second} {
					if err := store.Record(e); err != nil {
						t.Fatal(err)
					}
				}
				path := db.Path()
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store = clockedStore(db)
				if n, err := store.PruneEventsBefore(second.Timestamp.Add(time.Hour), batch); err != nil || n != 2 {
					t.Fatalf("alice prune: %d %v", n, err)
				}
				from, to := alice.Timestamp.Truncate(time.Hour), second.Timestamp.Add(time.Minute)
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
				if err != nil || costUnitTotal(units) != 4 {
					t.Fatalf("alice accounting: %v %v", units, err)
				}
			})
		}
	}
}

func TestFreePartitionsCannotDisappear(t *testing.T) {
	fields := []string{"input_tokens", "output_tokens", "cached_tokens", "cache_creation_tokens", "explicit_cache_read_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "image_input_tokens", "cached_image_input_tokens", "image_output_tokens"}
	for _, source := range []string{BucketQuotaUsageAggHourly, BucketUsageAggHourly} {
		for _, field := range append([]string{"all"}, fields...) {
			t.Run(source+"/"+field, func(t *testing.T) {
				db := testDB(t)
				store := clockedStore(db)
				e := fixedPlainEvent()
				e.NonBillable = true
				e.OutputTokens = 100
				e.CachedTokens = 100
				e.CacheCreationTokens = 100
				e.ExplicitCacheReadTokens = 10
				e.CacheCreation5mTokens = 10
				e.CacheCreation1hTokens = 10
				e.ImageInputTokens = 10
				e.CachedImageInputTokens = 10
				e.ImageOutputTokens = 10
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(source))
					key := quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)
					if source == BucketUsageAggHourly {
						key = aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target)
					}
					var cell map[string]json.RawMessage
					if err := json.Unmarshal(b.Get(key), &cell); err != nil {
						return err
					}
					if field == "all" {
						delete(cell, "non_billable")
					} else {
						var subset map[string]json.RawMessage
						if err := json.Unmarshal(cell["non_billable"], &subset); err != nil {
							return err
						}
						delete(subset, field)
						encoded, err := json.Marshal(subset)
						if err != nil {
							return err
						}
						cell["non_billable"] = encoded
					}
					encoded, err := json.Marshal(cell)
					if err != nil {
						return err
					}
					return b.Put(key, encoded)
				}); err != nil {
					t.Fatal(err)
				}
				from, to := e.Timestamp.Truncate(time.Hour), e.Timestamp.Add(time.Hour)
				filter := usage.Filter{ClientName: e.ClientName, From: &from, To: &to}
				if source == BucketUsageAggHourly {
					if _, err := store.Aggregate(filter, "hour", replayCosts); !errors.Is(err, ErrUsageHistoryIncomplete) {
						t.Fatalf("free aggregate accepted: %v", err)
					}
				} else if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("free quota accepted: %v %v", units, err)
				}
			})
		}
	}
}

func TestFreshProofRejectsUnknownQuarantineOwner(t *testing.T) {
	for _, damage := range []string{"missing", "conflict"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			bob := fixedPlainEvent()
			bob.ClientName = "bob"
			if err := store.Record(bob); err != nil {
				t.Fatal(err)
			}
			key := []byte(usageKey(bob.Timestamp, bob.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
					return err
				}
				if damage == "missing" {
					if err := tx.Bucket([]byte(rawOwnerBucket)).Delete(key); err != nil {
						return err
					}
				}
				value := []byte("{broken")
				if damage == "conflict" {
					value = []byte(fmt.Sprintf(`{"client_name":"alice","timestamp":%q,"input_tokens":"bad"}`, bob.Timestamp.Format(time.RFC3339Nano)))
				}
				return tx.Bucket([]byte(BucketUsageEvents)).Put(key, value)
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(bob.Timestamp.Add(time.Minute), 1); err != nil || n != 1 {
				t.Fatalf("prune: %d %v", n, err)
			}
			alice := fixedPlainEvent()
			alice.Timestamp = bob.Timestamp.Add(2 * time.Hour)
			alice.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
			if err := store.Record(alice); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(freshAttestationBucket))
				if b != nil && b.Get([]byte(usageKey(alice.Timestamp, alice.RecordID))) != nil {
					t.Error("ambiguous quarantine granted fresh proof")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(alice.Timestamp.Add(time.Hour), 1); err != nil || n != 1 {
				t.Fatalf("alice prune: %d %v", n, err)
			}
			from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unknown scope was accepted: %v", err)
			}
		})
	}
}

func TestFreePartitionPruneAndLegacyZeroOmission(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.NonBillable = true
	event.CachedTokens = 100
	event.ExplicitCacheReadTokens = 10
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 1); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || len(units) != 1 || units[0].CostUSD != 0 {
		t.Fatalf("free retained usage: %v %v", units, err)
	}
	var cell AggCell
	if err := json.Unmarshal([]byte(`{"input_tokens":0,"output_tokens":0,"cached_tokens":0,"cache_creation_tokens":0}`), &cell); err != nil {
		t.Fatal(err)
	}
	if quotaCellBelowRaw([]byte(`{"input_tokens":0,"output_tokens":0,"cached_tokens":0,"cache_creation_tokens":0}`), cell, AggCell{}) {
		t.Fatal("legacy zero free omissions rejected")
	}
}
