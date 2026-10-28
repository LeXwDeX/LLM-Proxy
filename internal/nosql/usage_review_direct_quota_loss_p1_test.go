package nosql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72DirectReadsDetectLostQuotaCell(t *testing.T) {
	for _, damage := range []string{"missing", "null"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if err := NewTargetStore(db).Create(config.Target{Name: event.Target, Endpoint: "https://example.com", APIKey: "test-key", UsageAdapter: config.UsageAdapterStandard, ModelMappings: []config.ModelMapping{{Upstream: event.Model}}}); err != nil {
				t.Fatal(err)
			}
			if err := NewModelGroupStore(db).Create(config.ModelGroup{Name: "affected", CostCoefficient: 1, Bindings: []config.QuotaGroupBinding{{Target: event.Target, Model: event.Model}}}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				for _, marker := range []string{usageAggBuiltMetaKey, quotaUsageAggBuiltMetaKey} {
					if err := meta.Put([]byte(marker), []byte(`{"processed":1}`)); err != nil {
						return err
					}
				}
				b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				if damage == "missing" {
					return b.Delete(key)
				}
				return b.Put(key, []byte("null"))
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour).Add(15*time.Minute), event.Timestamp.Truncate(time.Hour).Add(20*time.Minute)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			check := func(store *UsageStore) {
				if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("direct costs: %v %v", units, err)
				}
				if aggregate, err := store.Aggregate(filter, "hour", replayCosts); !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("direct aggregate: %+v %v", aggregate, err)
				}
				if quota, err := store.SumQuotaByClientRange(event.ClientName, from, to); len(quota) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("direct quota: %v %v", quota, err)
				}
				if cells, err := store.SumTargetModelRange(filter); len(cells) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("direct target model: %v %v", cells, err)
				}
				groupFilter := filter
				groupFilter.ModelGroups = []string{"affected"}
				if _, err := store.AggregateModelGroups(groupFilter, replayCosts, NewTargetStore(store.db), NewModelGroupStore(store.db)); !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Errorf("affected model group: %v", err)
				}
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, Model: "other", From: &from, To: &to}, replayCosts, false); err != nil {
					t.Errorf("unrelated model: %v", err)
				}
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); err != nil {
					t.Errorf("unrelated client: %v", err)
				}
				otherFrom, otherTo := from.Add(24*time.Hour), to.Add(24*time.Hour)
				if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &otherFrom, To: &otherTo}, replayCosts, false); err != nil {
					t.Errorf("unrelated hour: %v", err)
				}
			}
			check(store)
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			check(clockedStore(db))
		})
	}
}

func TestIssue72DirectReadsRejectUnprovenLegacyPrune(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, event)
	if usageReceiptCount(t, db) != 0 {
		t.Fatal("fixture unexpectedly has a receipt")
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		for _, marker := range []string{usageAggBuiltMetaKey, quotaUsageAggBuiltMetaKey} {
			if err := meta.Put([]byte(marker), []byte(`{"processed":1}`)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
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
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("unproven legacy cost: %v %v", units, err)
	}
	if quota, err := store.SumQuotaByClientRange(event.ClientName, from, to); len(quota) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("unproven legacy quota: %v %v", quota, err)
	}
}
