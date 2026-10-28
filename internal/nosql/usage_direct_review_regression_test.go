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

func TestDirectReviewDamage(t *testing.T) {
	for _, damage := range []string{"lower-tokens", "legacy-missing", "bad-raw", "bad-cell", "missing-receipt-owner", "missing-receipt-proof", "missing-fresh-model"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if damage == "missing-fresh-model" {
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if damage == "missing-receipt-owner" || damage == "missing-receipt-proof" {
				if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
					t.Fatalf("prune: %d %v", n, err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				q := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				switch damage {
				case "lower-tokens":
					var cell AggCell
					if err := json.Unmarshal(q.Get(key), &cell); err != nil {
						return err
					}
					cell.InputTokens = 0
					data, _ := json.Marshal(cell)
					return q.Put(key, data)
				case "legacy-missing":
					b := tx.Bucket([]byte(BucketUsageAggHourly))
					v2 := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
					legacy := legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
					if err := b.Put(legacy, b.Get(v2)); err != nil {
						return err
					}
					if err := b.Delete(v2); err != nil {
						return err
					}
					if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(quotaUsageAggBuiltMetaKey), []byte("1")); err != nil {
						return err
					}
					return q.Delete(key)
				case "bad-raw":
					value := fmt.Sprintf(`{"client_name":%q,"timestamp":%q,"target":%q,"model":%q,"input_tokens":"bad"}`, event.ClientName, event.Timestamp.Format(time.RFC3339Nano), event.Target, event.Model)
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), []byte(value))
				case "bad-cell":
					if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
						return err
					}
					return q.Put(key, []byte("{broken"))
				case "missing-receipt-owner":
					return tx.Bucket([]byte(receiptOwnersBucket)).Delete([]byte(usageKey(event.Timestamp, event.RecordID)))
				case "missing-receipt-proof":
					return tx.Bucket([]byte(rollupProofBucket)).Delete(rollupProofKey(BucketQuotaUsageAggHourly, key))
				case "missing-fresh-model":
					return tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(usageKey(event.Timestamp, event.RecordID)))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if damage == "missing-fresh-model" {
				filter.Model = event.Model
			}
			if damage == "bad-cell" {
				if cells, err := store.SumTargetModelRange(filter); len(cells) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
					t.Fatalf("bad cell accepted: %v %v", cells, err)
				}
				return
			}
			if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("damage accepted: %v %v", units, err)
			}
		})
	}
}

func TestDirectReviewProofIgnoresOtherModelCorruptRaw(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		value := fmt.Sprintf(`{"client_name":%q,"timestamp":%q,"target":%q,"model":"other","input_tokens":"bad"}`, event.ClientName, event.Timestamp.Format(time.RFC3339Nano), event.Target)
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")), []byte(value))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || len(units) != 1 || units[0].CostUSD != 2 {
		t.Fatalf("unrelated model: %v %v", units, err)
	}
}

func TestDirectReviewUnknownRawMustNotBeAssumedUnrelated(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")), []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unattributed damage assumed unrelated: %v %v", units, err)
	}
}

func TestDirectReviewForgedIndexMustNotProveOtherClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		rawKey := []byte(usageKey(event.Timestamp, event.RecordID))
		index := tx.Bucket([]byte(BucketUsageEventsByClient))
		hours := tx.Bucket([]byte(BucketUsageClientHours))
		other := event
		other.ClientName = "bob"
		state := append([]byte(nil), hours.Get(clientHourKey(event.ClientName, event.Timestamp))...)
		if err := index.Delete(clientEventIndexKey(event, rawKey)); err != nil {
			return err
		}
		if err := index.Put(clientEventIndexKey(other, rawKey), rawKey); err != nil {
			return err
		}
		if err := hours.Delete(clientHourKey(event.ClientName, event.Timestamp)); err != nil {
			return err
		}
		if err := hours.Put(clientHourKey(other.ClientName, other.Timestamp), state); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEvents)).Put(rawKey, []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("forged index hid damaged raw: %v %v", units, err)
	}
}

func TestDirectReviewAllBillingPartitions(t *testing.T) {
	for _, field := range []string{"explicit_cache_read_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "image_input_tokens", "cached_image_input_tokens", "image_output_tokens", "non_billable"} {
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
				bucket := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				var cell map[string]any
				if err := json.Unmarshal(bucket.Get(key), &cell); err != nil {
					return err
				}
				if field == "non_billable" {
					cell[field] = map[string]int64{"input_tokens": event.InputTokens}
				} else {
					cell[field] = 0
				}
				data, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				return bucket.Put(key, data)
			}); err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("billing field %s accepted: %v %v", field, units, err)
			}
		})
	}
}
