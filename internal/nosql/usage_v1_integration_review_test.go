package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestLegacyV1UnmarkedIndexCostEvidence(t *testing.T) {
	for _, damage := range []string{"none", "swap", "hour-missing", "hour-corrupt", "hour-digest", "pointer", "index-delete", "extra-raw"} {
		t.Run(damage, func(t *testing.T) {
			db, store, resolver, day := cursorCostFixture(t)
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				return raw.ForEach(func(k, v []byte) error {
					var e usage.Event
					if err := json.Unmarshal(v, &e); err != nil {
						return err
					}
					return addClientEventIndex(tx, e, k, v)
				})
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				if meta.Get([]byte(usageClientIndexDone)) != nil || meta.Get([]byte(usageClientIndexCursor)) != nil || meta.Get([]byte(indexProgressV2)) != nil || meta.Get([]byte(indexRequiredV2)) != nil || tx.Bucket([]byte(indexProofsV2)) != nil {
					t.Fatal("v1 fixture has progress evidence")
				}
				raw := tx.Bucket([]byte(BucketUsageEvents))
				return raw.ForEach(func(k, _ []byte) error {
					_, owned, err := readRawOwner(tx, k)
					if err != nil {
						return err
					}
					if owned {
						t.Fatal("legacy row has owner")
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				c := raw.Cursor()
				k, v := c.First()
				k, v = bytes.Clone(k), bytes.Clone(v)
				var e usage.Event
				if err := json.Unmarshal(v, &e); err != nil {
					return err
				}
				switch damage {
				case "swap":
					k2, v2 := c.Next()
					k2, v2 = bytes.Clone(k2), bytes.Clone(v2)
					var e2 usage.Event
					if err := json.Unmarshal(v2, &e2); err != nil {
						return err
					}
					e.InputTokens, e2.InputTokens = e2.InputTokens, e.InputTokens
					v, err := json.Marshal(e)
					if err != nil {
						return err
					}
					v2, err = json.Marshal(e2)
					if err != nil {
						return err
					}
					if err = raw.Put(k, v); err != nil {
						return err
					}
					return raw.Put(k2, v2)
				case "hour-missing":
					return tx.Bucket([]byte(BucketUsageClientHours)).Delete(clientHourKey("alice", e.Timestamp))
				case "hour-corrupt":
					return tx.Bucket([]byte(BucketUsageClientHours)).Put(clientHourKey("alice", e.Timestamp), []byte("{}bad"))
				case "hour-digest":
					state, err := loadClientHour(tx, clientHourKey("alice", e.Timestamp))
					if err != nil {
						return err
					}
					state.Digest[0] ^= 1
					data, err := json.Marshal(state)
					if err != nil {
						return err
					}
					return tx.Bucket([]byte(BucketUsageClientHours)).Put(clientHourKey("alice", e.Timestamp), data)
				case "pointer":
					return tx.Bucket([]byte(BucketUsageEventsByClient)).Put(clientEventIndexKey(e, k), []byte("missing"))
				case "index-delete":
					return tx.Bucket([]byte(BucketUsageEventsByClient)).Delete(clientEventIndexKey(e, k))
				case "extra-raw":
					e.Timestamp = day.Add(9*time.Hour + 20*time.Minute)
					e.InputTokens = 0
					data, err := json.Marshal(e)
					if err != nil {
						return err
					}
					return raw.Put([]byte(usageKey(e.Timestamp, "extra")), data)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			buckets := []string{BucketMeta, BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageAggHourly, BucketQuotaUsageAggHourly}
			before := map[string]map[string]string{}
			for _, name := range buckets {
				before[name] = indexBucketSnapshot(t, db, name)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if damage == "none" {
					if err != nil || costUnitTotal(units) != 21 {
						t.Errorf("client=%q cost=%v err=%v", client, costUnitTotal(units), err)
					}
				} else if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("client=%q accepted %s: %+v err=%v", client, damage, units, err)
				}
			}
			if damage == "none" {
				summary, err := store.summaryRead(day.Add(10*time.Hour+30*time.Minute), usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}, costReadHooks{})
				if err != nil || summary.LastHour.EstimatedCost != 2 {
					t.Errorf("summary=%+v err=%v", summary.LastHour, err)
				}
			}
			for _, name := range buckets {
				if !reflect.DeepEqual(before[name], indexBucketSnapshot(t, db, name)) {
					t.Errorf("cost read changed %s", name)
				}
			}
		})
	}
}

func TestDirectIntegrityMixedLegacyAndV2UsageCells(t *testing.T) {
	for _, damage := range []string{"none", "missing", "tampered", "malformed", "legacy-collision"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			first := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000, NonBillable: true}
			if damage == "legacy-collision" {
				first.ClientName = "alice|service"
				first.NonBillable = false
			}
			second := first
			second.Timestamp = day.Add(9*time.Hour + 45*time.Minute)
			second.InputTokens = 2_000_000
			second.NonBillable = false
			putLegacyClientEvents(t, db, []usage.Event{first})
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketUsageAggHourly))
				v2 := aggKey(first.Timestamp, first.EndpointType, first.ClientName, first.Model, first.Target)
				data := bytes.Clone(b.Get(v2))
				if err := b.Delete(v2); err != nil {
					return err
				}
				return b.Put(legacyAggKey(first.Timestamp, first.EndpointType, first.ClientName, first.Model, first.Target), data)
			}); err != nil {
				t.Fatal(err)
			}
			putLegacyClientEvents(t, db, []usage.Event{second})
			if damage == "legacy-collision" {
				if err := db.Update(func(tx *bolt.Tx) error {
					b := tx.Bucket([]byte(BucketUsageAggHourly))
					key := legacyAggKey(first.Timestamp, first.EndpointType, first.ClientName, first.Model, first.Target)
					if err := b.Delete(key); err != nil {
						return err
					}
					event := first
					event.ClientName = "alice"
					event.Model = "service"
					event.Target = "qwen-max|upstream"
					event.InputTokens = 100_000_000
					event.NonBillable = false
					var cell AggCell
					accumulateAggCell(&cell, event)
					data, err := json.Marshal(cell)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketUsageAggHourly))
				key := legacyAggKey(first.Timestamp, first.EndpointType, first.ClientName, first.Model, first.Target)
				switch damage {
				case "missing":
					return b.Delete(key)
				case "tampered":
					var cell AggCell
					if err := json.Unmarshal(b.Get(key), &cell); err != nil {
						return err
					}
					cell.InputTokens = 0
					cell.NonBillable = nil
					data, err := json.Marshal(cell)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				case "malformed":
					return b.Put(key, []byte("{bad"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			err := db.View(func(tx *bolt.Tx) error {
				return checkDirectQuotaEvidenceContext(context.Background(), tx, usage.Filter{ClientName: first.ClientName}, from, to, nil, nil)
			})
			if damage == "none" {
				if err != nil {
					t.Fatalf("mixed legacy+v2 rejected: %v", err)
				}
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, usage.CostTable{"qwen-max": {InputPer1MTokens: 1}}, true)
				if err != nil || costUnitTotal(units) != 2 {
					t.Errorf("cost=%v err=%v", costUnitTotal(units), err)
				}
			} else if !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Errorf("%s accepted: %v", damage, err)
			}
		})
	}
}
