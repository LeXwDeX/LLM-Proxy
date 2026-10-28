package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestRawOwnerDamagedEvidenceFailsClosed(t *testing.T) {
	for _, damage := range []string{"owner-missing", "marker-missing", "bucket-missing", "both-missing", "owner-moved", "owner-corrupt"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			event.ClientName = "bob"
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			key := []byte(usageKey(event.Timestamp, event.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				owner := tx.Bucket([]byte("usage_raw_owners_v1"))
				if owner == nil {
					return errors.New("raw owner evidence missing at write")
				}
				if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken")); err != nil {
					return err
				}
				switch damage {
				case "owner-missing":
					return owner.Delete(key)
				case "marker-missing":
					return tx.Bucket([]byte(BucketMeta)).Delete([]byte("usage_raw_owners_initialized_v1"))
				case "bucket-missing":
					return tx.DeleteBucket([]byte("usage_raw_owners_v1"))
				case "both-missing":
					if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte("usage_raw_owners_initialized_v1")); err != nil {
						return err
					}
					return tx.DeleteBucket([]byte("usage_raw_owners_v1"))
				case "owner-moved":
					value := append([]byte(nil), owner.Get(key)...)
					if err := owner.Delete(key); err != nil {
						return err
					}
					return owner.Put([]byte(usageKey(event.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")), value)
				default:
					return owner.Put(key, []byte(`{"version":1}`))
				}
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
			if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("bad owner accepted: %v %v", units, err)
			}
		})
	}
}

func TestRawOwnerSurvivesPruneAndReopen(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	var before []byte
	if err := db.View(func(tx *bolt.Tx) error {
		before = append([]byte(nil), tx.Bucket([]byte(rawOwnerBucket)).Get(key)...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
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
	if err := db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(before, tx.Bucket([]byte(rawOwnerBucket)).Get(key)) {
			t.Error("prune changed owner evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := clockedStore(db).RecordExists(event); !exists || err != nil {
		t.Fatalf("pruned lookup: %v %v", exists, err)
	}
}

func TestRawOwnerReindexDoesNotEraseEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	var owner []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		before := append([]byte(nil), raw.Get(key)...)
		owner = append([]byte(nil), tx.Bucket([]byte(rawOwnerBucket)).Get(key)...)
		changed := event
		changed.InputTokens++
		after, err := json.Marshal(changed)
		if err != nil {
			return err
		}
		if err := ReindexUsageEvent(tx, key, before, after); err != nil {
			return err
		}
		return raw.Put(key, after)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(owner, tx.Bucket([]byte(rawOwnerBucket)).Get(key)) {
			t.Error("reindex changed owner evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.RecordExists(event); exists || err == nil {
		t.Fatalf("changed raw accepted: %v %v", exists, err)
	}
}

func TestRawOwnerQuarantinePreservesEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	var before []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		before = append([]byte(nil), tx.Bucket([]byte(rawOwnerBucket)).Get(key)...)
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("quarantine: %d %v", n, err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(before, tx.Bucket([]byte(rawOwnerBucket)).Get(key)) || tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get(key) == nil {
			t.Error("quarantine lost evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.RecordExists(event); exists || err == nil {
		t.Fatalf("quarantined lookup: %v %v", exists, err)
	}
}

func TestRawOwnerWithoutRawOrReceiptCannotBeReused(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(usageKey(event.Timestamp, event.RecordID)))
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.RecordExists(event); exists || err == nil {
		t.Fatalf("owner-only lookup: %v %v", exists, err)
	}
	if added, err := store.RecordOnce(event); added || err == nil {
		t.Fatalf("owner-only retry: %v %v", added, err)
	}
}

func TestRawOwnerAtomicRollbackAfterAggregateFailure(t *testing.T) {
	db := testDB(t)
	event := fixedPlainEvent()
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	aggKey := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey, []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return writeUsageRowTx(tx, event, string(key)) }); err == nil {
		t.Fatal("corrupt aggregate did not fail the transaction")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEvents)).Get(key) != nil || tx.Bucket([]byte(rawOwnerBucket)) != nil || tx.Bucket([]byte(BucketMeta)).Get([]byte(rawOwnerMarker)) != nil || tx.Bucket([]byte(BucketMeta)).Get([]byte(rawOwnerRequired)) != nil {
			t.Error("failed write left raw or owner evidence")
		}
		if !bytes.Equal(tx.Bucket([]byte(BucketUsageAggHourly)).Get(aggKey), []byte("{broken")) {
			t.Error("failed write changed existing aggregate")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRawOwnerCopiedBindingCannotIdentifyAnotherKey(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	bob.RecordID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	aliceKey := []byte(usageKey(alice.Timestamp, alice.RecordID))
	bobKey := []byte(usageKey(bob.Timestamp, bob.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		owners := tx.Bucket([]byte(rawOwnerBucket))
		copy := append([]byte(nil), owners.Get(bobKey)...)
		if err := owners.Put(aliceKey, copy); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEvents)).Put(aliceKey, []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: alice.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("copied owner binding accepted: %v %v", units, err)
	}
}

func TestRawOwnerRecordExistsIsReadOnly(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	beforeID := issue72ReadTransactionID(t, db)
	before := indexBucketSnapshot(t, db, rawOwnerBucket)
	if exists, err := store.RecordExists(event); !exists || err != nil {
		t.Fatalf("lookup: %v %v", exists, err)
	}
	if after := issue72ReadTransactionID(t, db); after != beforeID {
		t.Fatalf("lookup wrote a transaction: %d -> %d", beforeID, after)
	}
	after := indexBucketSnapshot(t, db, rawOwnerBucket)
	if len(before) != len(after) {
		t.Fatal("lookup changed owner entries")
	}
	for key, value := range before {
		if after[key] != value {
			t.Fatal("lookup changed owner evidence")
		}
	}
}
