package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIndexedCostRejectsUnindexedZeroTokenOrphan(t *testing.T) {
	db, store, resolver, day := cursorCostFixture(t)
	migrateClientIndex(t, db)
	proofs := indexBucketSnapshot(t, db, indexProofsV2)
	raw := indexBucketSnapshot(t, db, BucketUsageEvents)
	rollups := indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
		if err != nil || costUnitTotal(units) != 21 {
			t.Fatalf("client=%q baseline cost=%v err=%v", client, costUnitTotal(units), err)
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		orphan := usage.Event{Timestamp: day.Add(9*time.Hour + 50*time.Minute), RecordID: "orphan", ClientName: "alice", Target: "upstream", Model: "qwen-max"}
		data, err := json.Marshal(orphan)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(orphan.Timestamp, orphan.RecordID)), data)
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(proofs, indexBucketSnapshot(t, db, indexProofsV2)) || !reflect.DeepEqual(rollups, indexBucketSnapshot(t, db, BucketQuotaUsageAggHourly)) {
		t.Fatal("orphan modified proofs or token totals")
	}
	retained := indexBucketSnapshot(t, db, BucketUsageEvents)
	for key, value := range raw {
		if retained[key] != value {
			t.Fatal("orphan modified proven raw")
		}
	}
	if len(retained) != len(raw)+1 {
		t.Fatal("orphan fixture has unexpected raw rows")
	}
	for _, client := range []string{"alice", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
		if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 || !strings.Contains(err.Error(), "client/hour evidence disagrees with raw history") {
			t.Errorf("client=%q accepted orphan: units=%+v err=%v", client, units, err)
		}
	}
}

func TestIndexedCostRejectsUnindexedRawWithEqualTotals(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	first := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	second := first
	second.Timestamp = day.Add(9*time.Hour + 45*time.Minute)
	second.InputTokens = 2_000_000
	putLegacyClientEvents(t, db, []usage.Event{first, second})
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		key, value := b.Cursor().First()
		key, value = append([]byte(nil), key...), append([]byte(nil), value...)
		first.InputTokens = 0
		after, err := json.Marshal(first)
		if err != nil {
			return err
		}
		if err := ReindexUsageEvent(tx, key, value, after); err != nil {
			return err
		}
		if err := b.Put(key, after); err != nil {
			return err
		}
		orphan := first
		orphan.Timestamp = day.Add(9*time.Hour + 20*time.Minute)
		orphan.InputTokens = 1_000_000
		data, err := json.Marshal(orphan)
		if err != nil {
			return err
		}
		return b.Put([]byte(usageKey(orphan.Timestamp, "orphan")), data)
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	got, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
		t.Fatalf("unindexed raw accepted: cost=%v units=%d err=%v", costUnitTotal(got), len(got), err)
	}
}

func TestGlobalCostDistinguishesMissingAndCorruptIndexEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		migrate, bad bool
	}{
		{"never-indexed", false, false},
		{"complete", true, false},
		{"marker-missing-intact", true, true},
		{"index-pointer-corrupt", true, true},
		{"state-corrupt-marker-missing", true, true},
		{"state-missing-marker-missing", true, true},
		{"index-missing-marker-missing", true, true},
		{"buckets-missing-marker-present", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			events := []usage.Event{{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
				{Timestamp: day.Add(9*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}}
			putLegacyClientEvents(t, db, events)
			if tc.migrate {
				migrateClientIndex(t, db)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				hours := tx.Bucket([]byte(BucketUsageClientHours))
				index := tx.Bucket([]byte(BucketUsageEventsByClient))
				key := clientHourKey("alice", events[0].Timestamp)
				switch tc.name {
				case "marker-missing-intact":
					return meta.Delete([]byte(usageClientIndexDone))
				case "index-pointer-corrupt":
					k, _ := index.Cursor().Seek([]byte(clientIndexPrefix("alice")))
					return index.Put(append([]byte(nil), k...), []byte("missing"))
				case "state-corrupt-marker-missing":
					if err := meta.Delete([]byte(usageClientIndexDone)); err != nil {
						return err
					}
					return hours.Put(key, []byte("{broken"))
				case "state-missing-marker-missing":
					if err := meta.Delete([]byte(usageClientIndexDone)); err != nil {
						return err
					}
					return hours.Delete(key)
				case "index-missing-marker-missing":
					if err := meta.Delete([]byte(usageClientIndexDone)); err != nil {
						return err
					}
					return tx.DeleteBucket([]byte(BucketUsageEventsByClient))
				case "buckets-missing-marker-present":
					if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
						return err
					}
					return tx.DeleteBucket([]byte(BucketUsageClientHours))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			got, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, r, true)
			if tc.bad {
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got) != 0 {
					t.Fatalf("corrupt evidence accepted: cost=%v err=%v", costUnitTotal(got), err)
				}
			} else if err != nil || costUnitTotal(got) != 21 {
				t.Fatalf("healthy cost=%v err=%v", costUnitTotal(got), err)
			}
		})
	}
}

func TestGlobalCostCanFallbackWhenOwnersProveIndexCorruption(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for _, e := range []usage.Event{{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}, {Timestamp: day.Add(9*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}} {
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEventsByClient))
		key, _ := b.Cursor().Seek([]byte(clientIndexPrefix("alice")))
		return b.Put(append([]byte(nil), key...), []byte("missing"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day, Mode: pricing.ModeSchedule, Timezone: "+00:00", BaseRates: usage.CostRates{InputPer1MTokens: 1}, Periods: []pricing.PricingPeriod{{Weekdays: []int{7}, Start: "09:30", End: "10:00", Rates: usage.CostRates{InputPer1MTokens: 10}}}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	got, err := store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(got) != 21 {
		t.Fatalf("cost=%v err=%v", costUnitTotal(got), err)
	}
}

func TestIndexedCostFallsBackForUnindexedOwnedRaw(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	for _, e := range []usage.Event{first, second} {
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(second.Timestamp, second.RecordID))
		return removeClientEventIndex(tx, key, tx.Bucket([]byte(BucketUsageEvents)).Get(key))
	}); err != nil {
		t.Fatal(err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	fallback := 0
	got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{scan: func(index bool) {
		if !index {
			fallback++
		}
	}})
	if err != nil || costUnitTotal(got.units) != 4 || len(got.units) != 2 || fallback != 1 {
		t.Fatalf("fallback=%d units=%+v err=%v", fallback, got.units, err)
	}
}

func TestIndexedCostCancellationDuringRawEvidenceScan(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	visits := 0
	got, err := store.readCostUnits(ctx, usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{evidenceRaw: func() { visits++; cancel() }})
	if !errors.Is(err, context.Canceled) || len(got.units) != 0 || visits != 1 {
		t.Fatalf("units=%+v visits=%d err=%v", got.units, visits, err)
	}
}

func TestCostIndexEvidenceCoversFullHourButIsolatesOtherClients(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
		{Timestamp: day.Add(9*time.Hour + 20*time.Minute), ClientName: "alice", Target: "upstream", Model: "other", InputTokens: 20},
		{Timestamp: day.Add(9*time.Hour + 30*time.Minute), ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
	}
	putLegacyClientEvents(t, db, events)
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEventsByClient))
		key, _ := b.Cursor().Seek([]byte(clientIndexPrefix("bob")))
		return b.Put(append([]byte(nil), key...), []byte("missing"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(9*time.Hour + 30*time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: "alice", Model: "qwen-max", From: &from, To: &to}
	visited := 0
	got, err := NewUsageStore(db).readCostUnits(context.Background(), filter, r, true, nil, costReadHooks{evidenceRaw: func() { visited++ }})
	if err != nil || len(got.units) != 1 || visited != len(events) {
		t.Fatalf("unrelated client or model blocked query: units=%+v evidence rows=%d err=%v", got.units, visited, err)
	}
}
