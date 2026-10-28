package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func reviewRewrite(t *testing.T, tx *bolt.Tx, key, before []byte, after usage.Event, move bool) error {
	t.Helper()
	var old usage.Event
	if err := json.Unmarshal(before, &old); err != nil {
		return err
	}
	data, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if err := ReindexUsageEvent(tx, key, before, data); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, data); err != nil {
		return err
	}
	for _, cell := range []struct {
		name string
		key  []byte
	}{
		{BucketUsageAggHourly, aggKey(old.Timestamp, old.EndpointType, old.ClientName, old.Model, old.Target)},
		{BucketQuotaUsageAggHourly, quotaAggKey(old.Timestamp, old.ClientName, old.Target, old.Model)},
	} {
		b := tx.Bucket([]byte(cell.name))
		if move {
			if err := b.Delete(cell.key); err != nil {
				return err
			}
			if cell.name == BucketUsageAggHourly {
				err = bumpAggBucket(b, after)
			} else {
				err = bumpQuotaAggBucket(b, after)
			}
		} else {
			var value AggCell
			if err = json.Unmarshal(b.Get(cell.key), &value); err != nil {
				return err
			}
			value.InputTokens += after.InputTokens - old.InputTokens
			var data []byte
			data, err = json.Marshal(value)
			if err == nil {
				err = b.Put(cell.key, data)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func TestIndexV2LostAtomicProofCannotBeRecreated(t *testing.T) {
	for _, action := range []string{"reindex", "migration", "rewrite-after-loss"} {
		t.Run(action, func(t *testing.T) {
			db, store, resolver, day := cursorCostFixture(t)
			stopIndexV2AfterOne(t, db)
			var key, before []byte
			if err := db.Update(func(tx *bolt.Tx) error {
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				c.First()
				k, v := c.Next()
				key, before = bytes.Clone(k), bytes.Clone(v)
				return ReindexUsageEvent(tx, key, before, before)
			}); err != nil {
				t.Fatal(err)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if err != nil || costUnitTotal(units) != 21 {
					t.Fatalf("initial atomic client=%q cost=%v err=%v", client, costUnitTotal(units), err)
				}
			}
			if action != "rewrite-after-loss" {
				if err := db.Update(func(tx *bolt.Tx) error {
					var e usage.Event
					if err := json.Unmarshal(before, &e); err != nil {
						return err
					}
					e.InputTokens = 4_000_000
					return reviewRewrite(t, tx, key, before, e, false)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if action != "rewrite-after-loss" {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, resolver, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Fatalf("changed raw accepted before loss: %v %+v", err, units)
				}
			}
			if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(indexProofsV2)).Delete(key) }); err != nil {
				t.Fatal(err)
			}
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("lost proof client=%q accepted: %v %+v", client, err, units)
				}
			}
			buckets := []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageAggHourly, BucketQuotaUsageAggHourly, indexProofsV2, BucketMeta}
			snap := make(map[string]map[string]string)
			for _, b := range buckets {
				snap[b] = indexBucketSnapshot(t, db, b)
			}
			var err error
			if action == "migration" {
				err = MigrateUsageClientIndex(context.Background(), db, 1)
			} else {
				err = db.Update(func(tx *bolt.Tx) error {
					value := tx.Bucket([]byte(BucketUsageEvents)).Get(key)
					if action == "reindex" {
						return ReindexUsageEvent(tx, key, value, value)
					}
					var e usage.Event
					if err := json.Unmarshal(value, &e); err != nil {
						return err
					}
					e.InputTokens = 4_000_000
					return reviewRewrite(t, tx, key, value, e, false)
				})
			}
			if !errors.Is(err, ErrUsageClientIndexIncomplete) {
				t.Errorf("%s recreated proof: %v", action, err)
			}
			for _, b := range buckets {
				if !reflect.DeepEqual(snap[b], indexBucketSnapshot(t, db, b)) {
					t.Errorf("%s changed %s", action, b)
				}
			}
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("post recovery client=%q accepted: %v %+v", client, err, units)
				}
			}
		})
	}
}

func TestIndexV2PruneReportsEarlierCommittedBatch(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	first := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), RecordID: "0", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 1}
	moved := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "2", ClientName: "other", Target: "upstream", Model: "qwen-max", InputTokens: 1}
	putLegacyClientEvents(t, db, []usage.Event{first, moved, later})
	migrateClientIndex(t, db)
	key := []byte(usageKey(moved.Timestamp, moved.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		moved.ClientName = "bob"
		return reviewRewrite(t, tx, key, before, moved, true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	proofs := indexBucketSnapshot(t, db, indexProofsV2)
	n, err := store.PruneEventsBefore(day.Add(10*time.Hour), 1)
	if n != 1 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	if !reflect.DeepEqual(proofs, indexBucketSnapshot(t, db, indexProofsV2)) {
		t.Error("prune changed proofs")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(usageKey(first.Timestamp, first.RecordID))) != nil || tx.Bucket([]byte(BucketUsageEvents)).Get(key) == nil {
			t.Error("incorrect rows retained after partial prune")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2StableScopeAfterCursorPrune(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}, {Timestamp: day.Add(10*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}})
	stopIndexV2AfterOne(t, db)
	if n, err := store.PruneEventsBefore(day.Add(10*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	from, to := day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
	mixed := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}, {Model: "other", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	for _, tc := range []struct {
		name, client, model string
		r                   usage.RateResolver
		want                float64
	}{
		{"stable", "alice", "", usage.CostTable{"qwen-max": {InputPer1MTokens: 2}}, 4},
		{"empty", "nobody", "", usage.CostTable{"qwen-max": {InputPer1MTokens: 2}}, 0},
		{"mixed", "alice", "qwen-max", mixed, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: tc.client, Model: tc.model, From: &from, To: &to}, tc.r, true)
			if err != nil || costUnitTotal(units) != tc.want {
				t.Fatalf("cost=%v err=%v", costUnitTotal(units), err)
			}
		})
	}
}

func TestIndexV2MovedProofRejectsOldScope(t *testing.T) {
	for _, move := range []string{"client", "model"} {
		t.Run(move, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			other := usage.Event{Timestamp: day.Add(9*time.Hour + 20*time.Minute), ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
			later := other
			later.Timestamp = day.Add(10*time.Hour + 10*time.Minute)
			later.InputTokens = 3_000_000
			putLegacyClientEvents(t, db, []usage.Event{e, other, later})
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				key, value := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
				key, value = bytes.Clone(key), bytes.Clone(value)
				if move == "client" {
					e.ClientName = "bob"
				} else {
					e.Model = "other"
				}
				return reviewRewrite(t, tx, key, value, e, true)
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}, {Model: "other", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			filters := []usage.Filter{{ClientName: "alice"}, {ClientName: "bob"}, {}}
			if move == "model" {
				filters = []usage.Filter{{ClientName: "alice", Model: "qwen-max"}, {ClientName: "alice", Model: "other"}, {}}
			}
			for _, filter := range filters {
				filter.From, filter.To = &from, &to
				units, err := store.ReadCostUnits(context.Background(), filter, r, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("%s client=%q model=%q accepted: %v %+v", move, filter.ClientName, filter.Model, err, units)
				}
			}
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
			if err != nil || costUnitTotal(units) != 2 {
				t.Errorf("unrelated same window: cost=%v err=%v", costUnitTotal(units), err)
			}
			from, to = day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
			units, err = store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
			if err != nil || costUnitTotal(units) != 3 {
				t.Errorf("unrelated cost=%v err=%v", costUnitTotal(units), err)
			}
		})
	}
}

func TestIndexV2CorruptOwnedOtherModelIsScoped(t *testing.T) {
	for _, pointer := range []string{"healthy", "redirected", "nested"} {
		t.Run(pointer, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			b := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "b", InputTokens: 2_000_000}
			a := usage.Event{Timestamp: day.Add(9*time.Hour + 20*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", ClientName: "alice", Target: "upstream", Model: "a", InputTokens: 1_000_000}
			for _, e := range []usage.Event{b, a} {
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				key := []byte(usageKey(b.Timestamp, b.RecordID))
				if err := tx.Bucket([]byte(indexProofsV2)).Put(key, []byte("{}")); err != nil {
					return err
				}
				index := tx.Bucket([]byte(BucketUsageEventsByClient))
				indexKey := clientEventIndexKey(b, key)
				switch pointer {
				case "redirected":
					return index.Put(indexKey, []byte(usageKey(a.Timestamp, a.RecordID)))
				case "nested":
					if err := index.Delete(indexKey); err != nil {
						return err
					}
					_, err := index.CreateBucket(indexKey)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			proofAfter := indexBucketSnapshot(t, db, indexProofsV2)
			indexAfter := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "a", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}, {Model: "b", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, scope := range []struct{ client, model string }{{"alice", "a"}, {"alice", "b"}, {"", ""}} {
				for _, force := range []bool{false, true} {
					got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: scope.client, Model: scope.model, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
					units := got.units
					if scope.model == "a" {
						if err != nil || costUnitTotal(units) != 1 {
							t.Errorf("a cost=%v err=%v", costUnitTotal(units), err)
						}
					} else if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
						t.Errorf("client=%q model=%q accepted: %v %+v", scope.client, scope.model, err, units)
					}
				}
			}
			if !reflect.DeepEqual(proofAfter, indexBucketSnapshot(t, db, indexProofsV2)) || !reflect.DeepEqual(indexAfter, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) {
				t.Error("cost read modified proof or index")
			}
		})
	}
}

func TestIndexV2IntactTimestampRewritePreservesOriginalScope(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	anchor := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), ClientName: "anchor", Target: "upstream", Model: "other", InputTokens: 1}
	e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	putLegacyClientEvents(t, db, []usage.Event{anchor})
	if err := db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err = tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, e.RecordID)), data); err != nil {
			return err
		}
		if err = bumpAgg(tx, e); err != nil {
			return err
		}
		return bumpQuotaAgg(tx, e)
	}); err != nil {
		t.Fatal(err)
	}
	stopIndexV2AfterOne(t, db)
	key := []byte(usageKey(e.Timestamp, e.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		e.Timestamp = day.Add(11*time.Hour + 10*time.Minute)
		return reviewRewrite(t, tx, key, before, e, true)
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(11*time.Hour), day.Add(12*time.Hour-time.Nanosecond)
	var atomicProof []byte
	if err := db.View(func(tx *bolt.Tx) error {
		p, err := loadIndexProgress(tx)
		if err != nil {
			return err
		}
		data := tx.Bucket([]byte(BucketUsageEvents)).Get(key)
		proof, attributable, invalid, _, err := indexProofForRaw(context.Background(), tx, &indexRawReferences{tx: tx}, p, key, data, e)
		if err != nil || !attributable || invalid || proof.Origin != "atomic" {
			t.Fatalf("initial atomic proof=%+v attributable=%v invalid=%v err=%v", proof, attributable, invalid, err)
		}
		atomicProof = bytes.Clone(tx.Bucket([]byte(indexProofsV2)).Get(key))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		e.ClientName = "bob"
		return reviewRewrite(t, tx, key, before, e, true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(atomicProof, tx.Bucket([]byte(indexProofsV2)).Get(key)) {
			t.Error("rewrite changed original atomic proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func(s *UsageStore) {
		for _, client := range []string{"alice", "bob", ""} {
			for _, force := range []bool{false, true} {
				got, err := s.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
					t.Errorf("client=%q force=%v accepted: %+v err=%v", client, force, got.units, err)
				}
			}
		}
	}
	check(store)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	check(NewUsageStore(reopened))
}

func TestIndexV2MissingRoundTripProofCannotExcludeOriginalTime(t *testing.T) {
	for _, loss := range []string{"missing", "corrupt", "nested"} {
		t.Run(loss, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			anchor := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), ClientName: "anchor", Target: "upstream", Model: "other", InputTokens: 1}
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			putLegacyClientEvents(t, db, []usage.Event{anchor})
			if err := db.Update(func(tx *bolt.Tx) error {
				data, err := json.Marshal(e)
				if err != nil {
					return err
				}
				if err = tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, e.RecordID)), data); err != nil {
					return err
				}
				if err = bumpAgg(tx, e); err != nil {
					return err
				}
				return bumpQuotaAgg(tx, e)
			}); err != nil {
				t.Fatal(err)
			}
			stopIndexV2AfterOne(t, db)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			originalTime := e.Timestamp
			if err := db.Update(func(tx *bolt.Tx) error {
				before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
				e.Timestamp = day.Add(11*time.Hour + 10*time.Minute)
				return reviewRewrite(t, tx, key, before, e, true)
			}); err != nil {
				t.Fatal(err)
			}
			var proof []byte
			if err := db.View(func(tx *bolt.Tx) error {
				proof = bytes.Clone(tx.Bucket([]byte(indexProofsV2)).Get(key))
				var p indexProof
				if json.Unmarshal(proof, &p) != nil || p.Origin != "atomic" {
					t.Fatalf("initial proof=%+v", p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				before := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
				e.Timestamp = originalTime
				e.ClientName = "bob"
				return reviewRewrite(t, tx, key, before, e, true)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(indexProofsV2))
				if !bytes.Equal(proof, b.Get(key)) {
					t.Fatal("rewrite changed original proof")
				}
				if err := b.Delete(key); err != nil {
					return err
				}
				switch loss {
				case "corrupt":
					return b.Put(key, []byte("{}"))
				case "nested":
					_, err := b.CreateBucket(key)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			check := func(s *UsageStore) {
				for _, tc := range []struct {
					client string
					hour   int
				}{{"alice", 11}, {"bob", 9}, {"", 11}, {"", 9}} {
					from, to := day.Add(time.Duration(tc.hour)*time.Hour), day.Add(time.Duration(tc.hour+1)*time.Hour-time.Nanosecond)
					for _, force := range []bool{false, true} {
						got, err := s.readCostUnits(context.Background(), usage.Filter{ClientName: tc.client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
						if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
							t.Errorf("client=%q hour=%d force=%v accepted: %+v err=%v", tc.client, tc.hour, force, got.units, err)
						}
					}
				}
			}
			check(store)
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			check(NewUsageStore(reopened))
		})
	}
}

func TestIndexV2DamagedOwnedProofPreservesIndependentClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	damaged := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	other := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
	for _, e := range []usage.Event{damaged, other} {
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(indexProofsV2)).Put([]byte(usageKey(damaged.Timestamp, damaged.RecordID)), []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	for _, hour := range []int{9, 10} {
		from, to := day.Add(time.Duration(hour)*time.Hour), day.Add(time.Duration(hour+1)*time.Hour-time.Nanosecond)
		client := "alice"
		if hour == 10 {
			client = "charlie"
		}
		for _, force := range []bool{false, true} {
			got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
			if hour == 9 {
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
					t.Errorf("damaged accepted: %+v err=%v", got.units, err)
				}
			} else if err != nil || costUnitTotal(got.units) != 2 {
				t.Errorf("independent cost=%v err=%v", costUnitTotal(got.units), err)
			}
		}
	}
}

func TestIndexV2CorruptUnownedOtherModelCannotBeScoped(t *testing.T) {
	db := testDB(t)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{
		{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "b", InputTokens: 2_000_000},
		{Timestamp: day.Add(9*time.Hour + 20*time.Minute), ClientName: "alice", Target: "upstream", Model: "a", InputTokens: 1_000_000},
	})
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		key, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return tx.Bucket([]byte(indexProofsV2)).Put(key, []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "a", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := NewUsageStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "a", From: &from, To: &to}, r, true)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Fatalf("unowned proof accepted: units=%+v err=%v", units, err)
	}
}

type indexInitCancelContext struct {
	context.Context
	calls, cancelAt int
}

func (c *indexInitCancelContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestIndexV2InitializationHonorsCancellation(t *testing.T) {
	for _, step := range []int{3, 4} {
		t.Run(map[int]string{3: "index", 4: "hours"}[step], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			e := fixedPlainEvent()
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
			e.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
			e.Timestamp = e.Timestamp.Add(time.Second)
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
			meta := indexBucketSnapshot(t, db, BucketMeta)
			index := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
			hours := indexBucketSnapshot(t, db, BucketUsageClientHours)
			ctx := &indexInitCancelContext{Context: context.Background(), cancelAt: step}
			err := db.Update(func(tx *bolt.Tx) error { _, err := startIndexProgress(ctx, tx); return err })
			if !errors.Is(err, context.Canceled) || ctx.calls != step {
				t.Fatalf("calls=%d err=%v", ctx.calls, err)
			}
			if !reflect.DeepEqual(meta, indexBucketSnapshot(t, db, BucketMeta)) || !reflect.DeepEqual(index, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) || !reflect.DeepEqual(hours, indexBucketSnapshot(t, db, BucketUsageClientHours)) {
				t.Fatal("cancelled initialization changed evidence")
			}
			if err := db.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte(indexProofsV2)) != nil {
					t.Error("cancelled initialization created proofs")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndexV2MovedMissingProofRejectsOldScope(t *testing.T) {
	for _, phase := range []string{"complete", "atomic"} {
		for _, move := range []string{"client", "model"} {
			for _, loss := range []string{"missing", "bucket"} {
				t.Run(phase+"/"+move+"/"+loss, func(t *testing.T) {
					db := testDB(t)
					store := NewUsageStore(db)
					day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
					e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
					if phase == "atomic" {
						e.Timestamp = day.Add(9*time.Hour + 20*time.Minute)
						e.RecordID = "1"
					}
					anchor := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), RecordID: "0", ClientName: "anchor", Target: "upstream", Model: "anchor", InputTokens: 1}
					later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
					if phase == "atomic" {
						later.RecordID = "2"
						putLegacyClientEvents(t, db, []usage.Event{anchor, e, later})
						stopIndexV2AfterOne(t, db)
					} else {
						putLegacyClientEvents(t, db, []usage.Event{e, later})
						migrateClientIndex(t, db)
					}
					key := []byte(usageKey(e.Timestamp, e.RecordID))
					if err := db.Update(func(tx *bolt.Tx) error {
						v := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
						if phase == "atomic" {
							if err := ReindexUsageEvent(tx, key, v, v); err != nil {
								return err
							}
						}
						if move == "client" {
							e.ClientName = "bob"
						} else {
							e.Model = "other"
						}
						return reviewRewrite(t, tx, key, v, e, true)
					}); err != nil {
						t.Fatal(err)
					}
					r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}, {Model: "other", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
					outsideFrom, outsideTo := day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
					units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &outsideFrom, To: &outsideTo}, r, true)
					if err != nil || costUnitTotal(units) != 2 {
						t.Fatalf("outside before proof loss cost=%v err=%v", costUnitTotal(units), err)
					}
					if err := db.Update(func(tx *bolt.Tx) error {
						b := tx.Bucket([]byte(indexProofsV2))
						if err := b.Delete(key); err != nil {
							return err
						}
						if loss == "bucket" {
							_, err := b.CreateBucket(key)
							return err
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
					filters := []usage.Filter{{ClientName: "alice"}, {ClientName: "bob"}, {}}
					if move == "model" {
						filters = []usage.Filter{{ClientName: "alice", Model: "qwen-max"}, {ClientName: "alice", Model: "other"}, {}}
					}
					for _, f := range filters {
						f.From, f.To = &from, &to
						units, err := store.ReadCostUnits(context.Background(), f, r, true)
						if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
							t.Errorf("client=%q model=%q accepted: %v %+v", f.ClientName, f.Model, err, units)
						}
					}
					from, to = day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
					units, err = store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
					// Without the unowned row's proof its original hour is unknown.
					if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
						t.Errorf("outside window accepted unknown attribution: units=%+v err=%v", units, err)
					}
				})
			}
		}
	}
}

func TestIndexV2PruneRejectsRewrittenAttribution(t *testing.T) {
	for _, batch := range []int{1, 100} {
		t.Run(map[int]string{1: "single", 100: "batch"}[batch], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
			putLegacyClientEvents(t, db, []usage.Event{e, later})
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				k, v := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
				e.ClientName = "bob"
				return reviewRewrite(t, tx, bytes.Clone(k), bytes.Clone(v), e, true)
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			check := func() {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("alice accepted: %v %+v", err, units)
				}
			}
			check()
			buckets := []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageAggHourly, BucketQuotaUsageAggHourly, BucketUsageReceipts, indexProofsV2, BucketMeta}
			snap := map[string]map[string]string{}
			for _, b := range buckets {
				snap[b] = indexBucketSnapshot(t, db, b)
			}
			n, err := store.PruneEventsBefore(day.Add(10*time.Hour), batch)
			if n != 0 || err == nil {
				t.Errorf("prune=%d err=%v", n, err)
			}
			for _, b := range buckets {
				if !reflect.DeepEqual(snap[b], indexBucketSnapshot(t, db, b)) {
					t.Errorf("prune changed %s", b)
				}
			}
			check()
		})
	}
}

func TestIndexV2TimestampRewriteCostScopes(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
	putLegacyClientEvents(t, db, []usage.Event{e, later})
	migrateClientIndex(t, db)
	key := []byte(usageKey(e.Timestamp, e.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		v := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		return ReindexUsageEvent(tx, key, v, v)
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", ""} {
		for _, force := range []bool{false, true} {
			got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
			if err != nil || costUnitTotal(got.units) != 1 {
				t.Fatalf("no-op client=%q force=%v cost=%v err=%v", client, force, costUnitTotal(got.units), err)
			}
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		v := bytes.Clone(tx.Bucket([]byte(BucketUsageEvents)).Get(key))
		e.Timestamp = day.Add(11*time.Hour + 10*time.Minute)
		return reviewRewrite(t, tx, key, v, e, true)
	}); err != nil {
		t.Fatal(err)
	}
	check := func(s *UsageStore) {
		for _, hour := range []int{9, 11} {
			from, to := day.Add(time.Duration(hour)*time.Hour), day.Add(time.Duration(hour+1)*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				for _, force := range []bool{false, true} {
					got, err := s.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
					if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
						t.Errorf("hour=%d client=%q force=%v accepted: %v %+v", hour, client, force, err, got.units)
					}
				}
			}
		}
	}
	check(store)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	check(NewUsageStore(reopened))
}

func TestIndexV2PruneRejectsMissingOrNestedProof(t *testing.T) {
	for _, loss := range []string{"missing", "bucket"} {
		t.Run(loss, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 1}
			putLegacyClientEvents(t, db, []usage.Event{e, later})
			migrateClientIndex(t, db)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(indexProofsV2))
				if err := b.Delete(key); err != nil {
					return err
				}
				if loss == "bucket" {
					_, err := b.CreateBucket(key)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			buckets := []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours, BucketUsageReceipts, indexProofsV2}
			snap := map[string]map[string]string{}
			for _, b := range buckets {
				snap[b] = indexBucketSnapshot(t, db, b)
			}
			n, err := store.PruneEventsBefore(day.Add(10*time.Hour), 100)
			if n != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Errorf("prune=%d err=%v", n, err)
			}
			for _, b := range buckets {
				if !reflect.DeepEqual(snap[b], indexBucketSnapshot(t, db, b)) {
					t.Errorf("changed %s", b)
				}
			}
		})
	}
}

func TestIndexV2MissingOrNestedOwnedProofAllowsDisjointScope(t *testing.T) {
	for _, loss := range []string{"missing", "bucket"} {
		t.Run(loss, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			b := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "bob", Target: "upstream", Model: "b", InputTokens: 2_000_000}
			a := usage.Event{Timestamp: day.Add(9*time.Hour + 20*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", ClientName: "alice", Target: "upstream", Model: "a", InputTokens: 1_000_000}
			for _, e := range []usage.Event{b, a} {
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			migrateClientIndex(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				key := []byte(usageKey(b.Timestamp, b.RecordID))
				proofs := tx.Bucket([]byte(indexProofsV2))
				if err := proofs.Delete(key); err != nil {
					return err
				}
				if loss == "bucket" {
					_, err := proofs.CreateBucket(key)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "a", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}, {Model: "b", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", Model: "a", From: &from, To: &to}, r, true)
			if err != nil || costUnitTotal(units) != 1 {
				t.Fatalf("independent owner: cost=%v err=%v", costUnitTotal(units), err)
			}
			if loss == "bucket" {
				units, err = store.ReadCostUnits(context.Background(), usage.Filter{From: &from, To: &to}, r, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Fatalf("nested proof accepted: %v %+v", err, units)
				}
			}
		})
	}
}

func TestIndexV2MalformedMovedAttributionSurvivesQuarantine(t *testing.T) {
	for _, move := range []string{"client", "model"} {
		t.Run(move, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4", ClientName: "later", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
			putLegacyClientEvents(t, db, []usage.Event{e})
			if err := store.Record(later); err != nil {
				t.Fatal(err)
			}
			independent := usage.Event{Timestamp: day.Add(9*time.Hour + 20*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 3_000_000}
			if err := store.Record(independent); err != nil {
				t.Fatal(err)
			}
			migrateClientIndex(t, db)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			var proof, malformed []byte
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				before := bytes.Clone(raw.Get(key))
				proof = bytes.Clone(tx.Bucket([]byte(indexProofsV2)).Get(key))
				if move == "client" {
					e.ClientName = "bob"
				} else {
					e.Model = "other"
				}
				if err := reviewRewrite(t, tx, key, before, e, true); err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw.Get(key), &fields); err != nil {
					return err
				}
				fields["input_tokens"] = json.RawMessage(`"bad"`)
				var err error
				malformed, err = json.Marshal(fields)
				if err != nil {
					return err
				}
				return raw.Put(key, malformed)
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}, {Model: "other", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			check := func(s *UsageStore, label string) {
				t.Helper()
				filters := []usage.Filter{{ClientName: "alice"}, {ClientName: "bob"}, {}}
				if move == "model" {
					filters = []usage.Filter{{ClientName: "alice", Model: "qwen-max"}, {ClientName: "alice", Model: "other"}, {}}
				}
				for _, f := range filters {
					f.From, f.To = &from, &to
					for _, force := range []bool{false, true} {
						got, err := s.readCostUnits(context.Background(), f, r, true, nil, costReadHooks{forceLegacy: force})
						if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
							t.Errorf("%s client=%q model=%q force=%v accepted: %v %+v", label, f.ClientName, f.Model, force, err, got.units)
						}
					}
				}
				units, err := s.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
				if err != nil || costUnitTotal(units) != 3 {
					t.Errorf("%s independent client cost=%v err=%v", label, costUnitTotal(units), err)
				}
				outsideFrom, outsideTo := day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
				units, err = s.ReadCostUnits(context.Background(), usage.Filter{ClientName: "later", From: &outsideFrom, To: &outsideTo}, r, true)
				if err != nil || costUnitTotal(units) != 2 {
					t.Errorf("%s outside window cost=%v err=%v", label, costUnitTotal(units), err)
				}
			}
			check(store, "retained")
			n, err := store.PruneEventsBefore(day.Add(9*time.Hour+15*time.Minute), 100)
			if n != 1 || err != nil {
				t.Fatalf("quarantine prune=%d err=%v", n, err)
			}
			verify := func(db *bolt.DB) {
				t.Helper()
				if err := db.View(func(tx *bolt.Tx) error {
					if !bytes.Equal(proof, tx.Bucket([]byte(indexProofsV2)).Get(key)) {
						t.Error("proof changed")
					}
					var record usageQuarantineRecord
					if json.Unmarshal(tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get(key), &record) != nil || !bytes.Equal(record.RawValue, malformed) || tx.Bucket([]byte(BucketUsageEvents)).Get(key) != nil {
						t.Error("quarantine did not preserve raw bytes")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			verify(db)
			check(store, "quarantine")
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			verify(reopened)
			check(NewUsageStore(reopened), "reopen")
		})
	}
}

func TestIndexV2MalformedMissingOriginalProofCannotExcludeClient(t *testing.T) {
	for _, loss := range []string{"missing", "corrupt", "bucket"} {
		t.Run(loss, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "later", Target: "upstream", Model: "qwen-max", InputTokens: 1}
			putLegacyClientEvents(t, db, []usage.Event{e, later})
			migrateClientIndex(t, db)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				before := bytes.Clone(raw.Get(key))
				e.ClientName = "bob"
				if err := reviewRewrite(t, tx, key, before, e, true); err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw.Get(key), &fields); err != nil {
					return err
				}
				fields["input_tokens"] = json.RawMessage(`"bad"`)
				data, err := json.Marshal(fields)
				if err != nil {
					return err
				}
				if err := raw.Put(key, data); err != nil {
					return err
				}
				proofs := tx.Bucket([]byte(indexProofsV2))
				if err := proofs.Delete(key); err != nil {
					return err
				}
				if loss == "corrupt" {
					return proofs.Put(key, []byte("{}"))
				}
				if loss == "bucket" {
					_, err := proofs.CreateBucket(key)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			check := func(label string) {
				for _, client := range []string{"alice", "bob", "charlie", ""} {
					units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true)
					if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
						t.Errorf("%s client=%q accepted: %v %+v", label, client, err, units)
					}
				}
			}
			check("retained")
			if n, err := store.PruneEventsBefore(day.Add(9*time.Hour+15*time.Minute), 100); n != 1 || err != nil {
				t.Fatalf("quarantine=%d err=%v", n, err)
			}
			check("quarantine")
		})
	}
}

func TestIndexV2MalformedLostAtomicProofWithDriftedScope(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		for _, action := range []string{"reindex", "migration", "prune"} {
			if malformed && action != "prune" {
				continue
			}
			t.Run(map[bool]string{true: "malformed", false: "numeric"}[malformed]+"/"+action, func(t *testing.T) {
				db := testDB(t)
				store := clockedStore(db)
				day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
				anchor := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), RecordID: "0", ClientName: "anchor", Target: "upstream", Model: "other", InputTokens: 1}
				e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
				later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "2", ClientName: "later", Target: "upstream", Model: "qwen-max", InputTokens: 1}
				putLegacyClientEvents(t, db, []usage.Event{anchor, e, later})
				stopIndexV2AfterOne(t, db)
				key := []byte(usageKey(e.Timestamp, e.RecordID))
				if err := db.Update(func(tx *bolt.Tx) error {
					raw := tx.Bucket([]byte(BucketUsageEvents))
					before := bytes.Clone(raw.Get(key))
					if err := ReindexUsageEvent(tx, key, before, before); err != nil {
						return err
					}
					e.ClientName = "bob"
					if err := reviewRewrite(t, tx, key, before, e, true); err != nil {
						return err
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw.Get(key), &fields); err != nil {
						return err
					}
					fields["client_name"] = json.RawMessage(`"mallory"`)
					if malformed {
						fields["input_tokens"] = json.RawMessage(`"bad"`)
					}
					data, err := json.Marshal(fields)
					if err != nil {
						return err
					}
					if err := raw.Put(key, data); err != nil {
						return err
					}
					return tx.Bucket([]byte(indexProofsV2)).Delete(key)
				}); err != nil {
					t.Fatal(err)
				}
				r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
				from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
				check := func(label string) {
					units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
					if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
						t.Errorf("%s accepted drifted scope: %v %+v", label, err, units)
					}
				}
				check("retained")
				if malformed {
					if n, err := store.PruneEventsBefore(day.Add(9*time.Hour+15*time.Minute), 100); err != nil || n != 2 {
						t.Fatalf("prune=%d err=%v", n, err)
					}
					check("quarantine")
				} else {
					switch action {
					case "reindex":
						if err := db.Update(func(tx *bolt.Tx) error {
							v := tx.Bucket([]byte(BucketUsageEvents)).Get(key)
							return ReindexUsageEvent(tx, key, v, v)
						}); !errors.Is(err, ErrUsageClientIndexIncomplete) {
							t.Errorf("reindex accepted lost proof: %v", err)
						}
					case "migration":
						proofs := indexBucketSnapshot(t, db, indexProofsV2)
						index := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
						meta := indexBucketSnapshot(t, db, BucketMeta)
						if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
							t.Errorf("migration accepted lost proof: %v", err)
						}
						if !reflect.DeepEqual(proofs, indexBucketSnapshot(t, db, indexProofsV2)) || !reflect.DeepEqual(index, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) || !reflect.DeepEqual(meta, indexBucketSnapshot(t, db, BucketMeta)) {
							t.Error("failed migration committed new evidence")
						}
					case "prune":
						if n, err := store.PruneEventsBefore(day.Add(9*time.Hour+15*time.Minute), 100); n != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
							t.Errorf("prune=%d err=%v", n, err)
						}
					}
					check("after recovery attempts")
				}
			})
		}
	}
}

func TestIndexV2MalformedReverseIndexCheckCancels(t *testing.T) {
	db, _, _, day := cursorCostFixture(t)
	stopIndexV2AfterOne(t, db)
	var key, malformed []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		c.First()
		k, v := c.Next()
		key = bytes.Clone(k)
		if err := ReindexUsageEvent(tx, key, v, v); err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(v, &fields); err != nil {
			return err
		}
		fields["client_name"] = json.RawMessage(`"other"`)
		fields["input_tokens"] = json.RawMessage(`"bad"`)
		var err error
		malformed, err = json.Marshal(fields)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(indexProofsV2)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	ctx := &indexInitCancelContext{Context: context.Background(), cancelAt: 2}
	err := db.View(func(tx *bolt.Tx) error {
		return checkMalformedIndexScope(ctx, tx, &indexRawReferences{tx: tx}, key, malformed, "alice", day, day.Add(24*time.Hour), "", nil)
	})
	if !errors.Is(err, context.Canceled) || ctx.calls != 2 {
		t.Fatalf("calls=%d err=%v", ctx.calls, err)
	}
}

func TestIndexV2MissingRewrittenRawPreservesProofScopes(t *testing.T) {
	for _, loss := range []string{"proof-intact", "proof-missing", "proof-corrupt", "index-nested", "index-redirected"} {
		t.Run(loss, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
			putLegacyClientEvents(t, db, []usage.Event{e, later})
			migrateClientIndex(t, db)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				v := bytes.Clone(raw.Get(key))
				e.ClientName = "bob"
				if err := reviewRewrite(t, tx, key, v, e, true); err != nil {
					return err
				}
				if err := raw.Delete(key); err != nil {
					return err
				}
				proofs := tx.Bucket([]byte(indexProofsV2))
				switch loss {
				case "proof-missing":
					return proofs.Delete(key)
				case "proof-corrupt":
					return proofs.Put(key, []byte("{}"))
				case "index-nested":
					index := tx.Bucket([]byte(BucketUsageEventsByClient))
					indexKey := clientEventIndexKey(e, key)
					if err := index.Delete(indexKey); err != nil {
						return err
					}
					_, err := index.CreateBucket(indexKey)
					return err
				case "index-redirected":
					return tx.Bucket([]byte(BucketUsageEventsByClient)).Put(clientEventIndexKey(e, key), []byte(usageKey(later.Timestamp, later.RecordID)))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			check := func(s *UsageStore) {
				from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
				for _, client := range []string{"alice", "bob", ""} {
					for _, force := range []bool{false, true} {
						got, err := s.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
						if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
							t.Errorf("client=%q force=%v accepted: %v %+v", client, force, err, got.units)
						}
					}
				}
				from, to = day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
				units, err := s.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
				if loss == "proof-intact" || loss == "index-nested" || loss == "index-redirected" {
					if err != nil || costUnitTotal(units) != 2 {
						t.Errorf("outside cost=%v err=%v", costUnitTotal(units), err)
					}
				} else if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("unknown attribution outside accepted: units=%+v err=%v", units, err)
				}
			}
			check(store)
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			check(NewUsageStore(reopened))
		})
	}
}

func TestIndexV2NestedIndexCannotRegenerateAtomicProof(t *testing.T) {
	for _, action := range []string{"reindex", "migration"} {
		t.Run(action, func(t *testing.T) {
			db, _, _, _ := cursorCostFixture(t)
			stopIndexV2AfterOne(t, db)
			var key []byte
			if err := db.Update(func(tx *bolt.Tx) error {
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				c.First()
				k, v := c.Next()
				key = bytes.Clone(k)
				if err := ReindexUsageEvent(tx, key, v, v); err != nil {
					return err
				}
				var event usage.Event
				if err := json.Unmarshal(v, &event); err != nil {
					return err
				}
				index := tx.Bucket([]byte(BucketUsageEventsByClient))
				indexKey := clientEventIndexKey(event, key)
				if err := index.Delete(indexKey); err != nil {
					return err
				}
				if _, err := index.CreateBucket(indexKey); err != nil {
					return err
				}
				return tx.Bucket([]byte(indexProofsV2)).Delete(key)
			}); err != nil {
				t.Fatal(err)
			}
			proofs := indexBucketSnapshot(t, db, indexProofsV2)
			var err error
			if action == "migration" {
				err = MigrateUsageClientIndex(context.Background(), db, 1)
			} else {
				err = db.Update(func(tx *bolt.Tx) error {
					value := tx.Bucket([]byte(BucketUsageEvents)).Get(key)
					return ReindexUsageEvent(tx, key, value, value)
				})
			}
			if !errors.Is(err, ErrUsageClientIndexIncomplete) {
				t.Errorf("%s accepted missing proof with nested index: %v", action, err)
			}
			if !reflect.DeepEqual(proofs, indexBucketSnapshot(t, db, indexProofsV2)) {
				t.Error("replacement proof committed")
			}
		})
	}
}

func TestIndexV2HealthyPruneDoesNotCreateOrphanConflict(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	first := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
	putLegacyClientEvents(t, db, []usage.Event{first, later})
	migrateClientIndex(t, db)
	if n, err := store.PruneEventsBefore(day.Add(10*time.Hour), 100); n != 1 || err != nil {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "charlie", From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("healthy retained cost=%v err=%v", costUnitTotal(units), err)
	}
}

func TestIndexV2OrphanIndependentOwnerAllowsDisjointScope(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	b := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", ClientName: "bob", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
	a := usage.Event{Timestamp: day.Add(9*time.Hour + 20*time.Minute), RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
	for _, e := range []usage.Event{b, a} {
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(usageKey(b.Timestamp, b.RecordID))
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(indexProofsV2)).Put(key, []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("independent cost=%v err=%v", costUnitTotal(units), err)
	}
	for _, client := range []string{"bob", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true)
		if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
			t.Errorf("client=%q accepted: %+v err=%v", client, units, err)
		}
	}
}

func TestIndexV2OrphanScanHonorsCancellation(t *testing.T) {
	db, _, _, day := cursorCostFixture(t)
	migrateClientIndex(t, db)
	ctx := &indexInitCancelContext{Context: context.Background(), cancelAt: 1}
	err := db.View(func(tx *bolt.Tx) error {
		p, err := loadIndexProgress(tx)
		if err != nil {
			return err
		}
		refs := &indexRawReferences{tx: tx}
		return refs.checkOrphans(ctx, p, usage.Filter{}, day, day.Add(time.Hour), nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func BenchmarkIndexV2LegacyMigrationReview(b *testing.B) {
	for _, count := range []int{2000, 8000} {
		b.Run(fmt.Sprintf("rows-%d", count), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				db := testDB(b)
				at := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
				events := make([]usage.Event, count)
				for n := range events {
					events[n] = usage.Event{Timestamp: at.Add(time.Duration(n) * time.Second), ClientName: fmt.Sprintf("client-%03d", n%20), Model: "qwen-max", InputTokens: 1}
				}
				putLegacyClientEvents(b, db, events)
				b.StartTimer()
				err := MigrateUsageClientIndex(context.Background(), db, 256)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestIndexV2OrphanRejectsDamagedSourcesAndAttribution(t *testing.T) {
	for _, damage := range []string{"raw-source", "index-source", "malformed-index", "unknown-timestamp"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			e := usage.Event{Timestamp: day.Add(9*time.Hour + 10*time.Minute), RecordID: "0", ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000}
			later := usage.Event{Timestamp: day.Add(10*time.Hour + 10*time.Minute), RecordID: "1", ClientName: "charlie", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000}
			if damage == "unknown-timestamp" {
				anchor := usage.Event{Timestamp: day.Add(9*time.Hour + 5*time.Minute), ClientName: "anchor", Target: "upstream", Model: "other", InputTokens: 1}
				putLegacyClientEvents(t, db, []usage.Event{anchor})
				e.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
				if err := db.Update(func(tx *bolt.Tx) error {
					data, err := json.Marshal(e)
					if err != nil {
						return err
					}
					if err = tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, e.RecordID)), data); err != nil {
						return err
					}
					if err = bumpAgg(tx, e); err != nil {
						return err
					}
					return bumpQuotaAgg(tx, e)
				}); err != nil {
					t.Fatal(err)
				}
				stopIndexV2AfterOne(t, db)
			} else {
				putLegacyClientEvents(t, db, []usage.Event{e, later})
				migrateClientIndex(t, db)
			}
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				before := bytes.Clone(raw.Get(key))
				if damage == "unknown-timestamp" {
					e.Timestamp = day.Add(11*time.Hour + 10*time.Minute)
					if err := reviewRewrite(t, tx, key, before, e, true); err != nil {
						return err
					}
					before = bytes.Clone(raw.Get(key))
				}
				e.ClientName = "bob"
				if err := reviewRewrite(t, tx, key, before, e, true); err != nil {
					return err
				}
				switch damage {
				case "raw-source":
					return tx.DeleteBucket([]byte(BucketUsageEvents))
				case "index-source":
					return tx.DeleteBucket([]byte(BucketUsageEventsByClient))
				case "malformed-index":
					index := tx.Bucket([]byte(BucketUsageEventsByClient))
					old := clientEventIndexKey(e, key)
					if err := index.Delete(old); err != nil {
						return err
					}
					if err := index.Put(append([]byte(clientIndexPrefix("bob")+"not-a-time|"), key...), key); err != nil {
						return err
					}
				case "unknown-timestamp":
					if err := tx.Bucket([]byte(indexProofsV2)).Delete(key); err != nil {
						return err
					}
				}
				return raw.Delete(key)
			}); err != nil {
				t.Fatal(err)
			}
			r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 1}}}, nil)
			check := func(s *UsageStore) {
				hour := 9
				if damage == "unknown-timestamp" {
					hour = 11
				}
				from, to := day.Add(time.Duration(hour)*time.Hour), day.Add(time.Duration(hour+1)*time.Hour-time.Nanosecond)
				for _, client := range []string{"alice", "bob", ""} {
					for _, force := range []bool{false, true} {
						got, err := s.readCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, r, true, nil, costReadHooks{forceLegacy: force})
						if !errors.Is(err, ErrUsageHistoryIncomplete) || len(got.units) != 0 {
							t.Errorf("client=%q force=%v accepted: %v %+v", client, force, err, got.units)
						}
					}
				}
			}
			check(store)
			if damage == "malformed-index" {
				path := db.Path()
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				check(NewUsageStore(reopened))
			}
		})
	}
}

func TestIndexV2AbsentSourcesDoNotAlterLegacyOrphanCheck(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(BucketUsageEvents)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageEventsByClient))
	}); err != nil {
		t.Fatal(err)
	}
	err := db.View(func(tx *bolt.Tx) error {
		p, err := loadIndexProgress(tx)
		if err != nil || p != nil {
			t.Fatalf("legacy progress=%+v err=%v", p, err)
		}
		refs := &indexRawReferences{tx: tx}
		return refs.checkOrphans(context.Background(), p, usage.Filter{}, time.Time{}, time.Now(), nil)
	})
	if err != nil {
		t.Fatal(err)
	}
}
