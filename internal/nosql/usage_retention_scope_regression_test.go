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

func TestRetentionReviewUntrustedPayloadScope(t *testing.T) {
	for _, damage := range []string{"model", "timestamp"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			at := event.Timestamp
			if damage == "timestamp" {
				at = at.Add(48 * time.Hour)
			}
			value := []byte(fmt.Sprintf(`{"client_name":%q,"timestamp":%q,"model":"other","input_tokens":"bad"}`, event.ClientName, at.Format(time.RFC3339Nano)))
			if err := db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), value); err != nil {
					return err
				}
				return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
				t.Fatalf("prune: %d %v", n, err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				var record usageQuarantineRecord
				if err := json.Unmarshal(tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get([]byte(usageKey(event.Timestamp, event.RecordID))), &record); err != nil {
					return err
				}
				if !bytes.Equal(record.RawValue, value) {
					t.Error("original raw bytes changed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Minute)
			filter := usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}
			if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("original scope: %v %v", units, err)
			}
			filter.ClientName = "bob"
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated client: %v", err)
			}
			filter.ClientName = event.ClientName
			laterFrom, laterTo := from.Add(24*time.Hour), to.Add(24*time.Hour)
			filter.From, filter.To = &laterFrom, &laterTo
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated hour: %v", err)
			}
		})
	}
}

func TestRetentionReviewQuarantineDoesNotPoisonOtherOwner(t *testing.T) {
	for _, batch := range []int{1, 100} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			bob := fixedPlainEvent()
			bob.ClientName = "bob"
			bob.Timestamp = bob.Timestamp.Add(-time.Minute)
			alice := fixedPlainEvent()
			alice.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
			for _, e := range []usage.Event{bob, alice} {
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(bob.Timestamp, bob.RecordID)), []byte("{broken"))
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(alice.Timestamp.Add(time.Hour), batch); err != nil || n != 2 {
				t.Fatalf("prune %d %v", n, err)
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
			from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
			if err != nil || len(units) != 1 || units[0].CostUSD != 2 {
				t.Fatalf("alice: %v %v", units, err)
			}
		})
	}
}

func TestRetentionReviewDeletedOptionalPartitions(t *testing.T) {
	for _, field := range []string{"explicit_cache_read_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "image_input_tokens", "cached_image_input_tokens", "image_output_tokens"} {
		t.Run(field, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			event.OutputTokens = 100
			event.CachedTokens = 100
			event.CacheCreationTokens = 100
			event.ExplicitCacheReadTokens = 10
			event.CacheCreation5mTokens = 10
			event.CacheCreation1hTokens = 10
			event.ImageInputTokens = 10
			event.CachedImageInputTokens = 10
			event.ImageOutputTokens = 10
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				var cell map[string]json.RawMessage
				if err := json.Unmarshal(b.Get(key), &cell); err != nil {
					return err
				}
				delete(cell, field)
				value, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				return b.Put(key, value)
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("deleted field accepted: %v %v", units, err)
			}
		})
	}
}

func TestRetentionReviewDeletedUsageAggregatePartition(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.CachedTokens = 100
	event.ExplicitCacheReadTokens = 10
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		key := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
		var cell map[string]json.RawMessage
		if err := json.Unmarshal(b.Get(key), &cell); err != nil {
			return err
		}
		delete(cell, "explicit_cache_read_tokens")
		value, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return b.Put(key, value)
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.Aggregate(usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, "hour", replayCosts); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("aggregate accepted deleted partition: %v", err)
	}
}

func TestRetentionReviewQuarantineUnknownOwnerDoesNotProveOtherClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	bob := fixedPlainEvent()
	bob.ClientName = "bob"
	bob.Timestamp = bob.Timestamp.Add(-time.Minute)
	alice := fixedPlainEvent()
	alice.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	for _, event := range []usage.Event{bob, alice} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(bob.Timestamp, bob.RecordID))
		if err := tx.Bucket([]byte(rawOwnerBucket)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(alice.Timestamp.Add(time.Hour), 1); err != nil || n != 2 {
		t.Fatalf("prune %d %v", n, err)
	}
	from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown quarantine accepted: %v %v", units, err)
	}
}

func TestRetentionReviewOptionalZeroAndFreeStayReadable(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprint(free), func(t *testing.T) {
			store := clockedStore(testDB(t))
			event := fixedPlainEvent()
			event.NonBillable = free
			if free {
				event.CachedTokens = 100
				event.ExplicitCacheReadTokens = 10
			}
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, usage.CostTable{}, false)
			if err != nil || len(units) != 1 || units[0].CostUSD != 0 {
				t.Fatalf("zero price or free rejected: %v %v", units, err)
			}
		})
	}
}
