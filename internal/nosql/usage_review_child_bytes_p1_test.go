package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72ChildFirstPrunePreservesUnknownChildBytes(t *testing.T) {
	for _, batch := range []int{1, 100} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			bundle := fixedIdentityBundle()
			bundle.RecordID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
			_, keys, err := normalizeUsageBundle(bundle)
			if err != nil || keys[1] >= keys[0] {
				t.Fatalf("child order: %v %v", keys, err)
			}
			if err := store.Record(bundle); err != nil {
				t.Fatal(err)
			}
			key := []byte(keys[1])
			var original []byte
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				data := raw.Get(key)
				original = append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"future_field":{"opaque":"original"}}`)...)
				return raw.Put(key, original)
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(bundle.Timestamp.Add(2*time.Second), batch); n != 2 || err != nil {
				t.Fatalf("prune: %d %v", n, err)
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			found := false
			if err := db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(unresolvedBucket))
				if b == nil {
					return nil
				}
				return b.ForEach(func(_, data []byte) error {
					var item struct {
						Kind        string `json:"kind"`
						Original    []byte `json:"original"`
						OriginalKey []byte `json:"original_key"`
					}
					if err := json.Unmarshal(data, &item); err != nil {
						return err
					}
					if item.Kind == "prune_unverified" && bytes.Equal(item.OriginalKey, key) && bytes.Equal(item.Original, original) {
						found = true
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Error("child original key and bytes were not preserved")
			}
			from, to := bundle.Timestamp.Truncate(time.Hour), bundle.Timestamp.Add(time.Hour)
			if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: bundle.ClientName, Model: "advisor", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unknown child accepted: %v", err)
			}
			if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
				t.Fatalf("unrelated client blocked: %v", err)
			}
		})
	}
}

func TestIssue72ChildFirstPruneEvidenceFailureRollsBack(t *testing.T) {
	for _, batch := range []int{1, 100} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			bundle := fixedIdentityBundle()
			bundle.RecordID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
			_, keys, err := normalizeUsageBundle(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Record(bundle); err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			key := []byte(keys[1])
			var original []byte
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				data := raw.Get(key)
				original = append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"future_field":true}`)...)
				if err := raw.Put(key, original); err != nil {
					return err
				}
				return tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedInitialized), []byte("1"))
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(bundle.Timestamp.Add(2*time.Second), batch); n != 0 || !errors.Is(err, ErrUsageAggregateSourceMissing) {
				t.Fatalf("evidence failure: %d %v", n, err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				if !bytes.Equal(tx.Bucket([]byte(BucketUsageEvents)).Get(key), original) {
					t.Error("failed evidence registration changed child raw bytes")
				}
				if tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[0])) == nil {
					t.Error("failed evidence registration deleted root")
				}
				if tx.Bucket([]byte(BucketUsageReceipts)).Get(key) != nil {
					t.Error("failed evidence registration retained child receipt")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
