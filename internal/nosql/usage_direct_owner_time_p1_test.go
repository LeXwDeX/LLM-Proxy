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

func TestDirectOwnerCorruptHistoryUsesBoundKeyHour(t *testing.T) {
	for _, tamper := range []string{"tokens", "model", "timestamp"} {
		t.Run(tamper, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			old := fixedPlainEvent()
			current := old
			current.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa9"
			current.Timestamp = old.Timestamp.Add(24 * time.Hour)
			for _, event := range []usage.Event{old, current} {
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			key := []byte(usageKey(old.Timestamp, old.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketUsageEvents))
				var stored usage.Event
				if err := json.Unmarshal(b.Get(key), &stored); err != nil {
					return err
				}
				stored.InputTokens++
				if tamper == "model" {
					stored.Model = "other"
				}
				if tamper == "timestamp" {
					stored.Timestamp = current.Timestamp
				}
				value, err := json.Marshal(stored)
				if err != nil {
					return err
				}
				return b.Put(key, value)
			}); err != nil {
				t.Fatal(err)
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
			from, to := current.Timestamp.Truncate(time.Hour), current.Timestamp.Add(time.Minute)
			for _, filter := range []usage.Filter{{From: &from, To: &to}, {ClientName: old.ClientName, From: &from, To: &to}, {ClientName: old.ClientName, Model: old.Model, From: &from, To: &to}} {
				units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
				if err != nil || len(units) != 1 || units[0].CostUSD != 2 {
					t.Errorf("current range %s: %v %v", fmt.Sprint(filter.ClientName, filter.Model), units, err)
				}
			}
			if result, err := store.Aggregate(usage.Filter{From: &from, To: &to}, "hour", replayCosts); err != nil || result.Totals.EstimatedCost != 2 {
				t.Errorf("global aggregate: %+v %v", result.Totals, err)
			}
			from, to = old.Timestamp.Truncate(time.Hour), old.Timestamp.Add(time.Minute)
			for _, model := range []string{"", old.Model, "other"} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: old.ClientName, Model: model, From: &from, To: &to}, replayCosts, false)
				if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("damaged hour model %q: %v %v", model, units, err)
				}
			}
		})
	}
}

func TestDirectOwnerInvalidBindingDoesNotExcludeOtherHour(t *testing.T) {
	for _, damage := range []string{"invalid", "missing"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			key := []byte(usageKey(event.Timestamp, event.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(rawOwnerBucket))
				if damage == "missing" {
					if err := b.Delete(key); err != nil {
						return err
					}
				} else if err := b.Put(key, []byte("{broken")); err != nil {
					return err
				}
				return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken"))
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Add(24*time.Hour), event.Timestamp.Add(25*time.Hour)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("unbound damage excluded: %v %v", units, err)
			}
		})
	}
}
