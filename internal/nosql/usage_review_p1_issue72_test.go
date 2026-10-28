package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72StableCostRejectsMalformedQuotaKey(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	hour := boundScanTime.Truncate(time.Hour)
	putQuotaAggRaw(t, db, hour.Format(hourLayout)+"|alice", []byte(`{"requests":1,"input_tokens":1000000}`))
	from, to := hour, hour.Add(time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, true); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("malformed stable-price rollup reported complete: %v %v", units, err)
	}
	from, to = hour.Add(24*time.Hour), hour.Add(25*time.Hour)
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, true); len(units) != 0 || err != nil {
		t.Fatalf("unrelated time blocked: %v %v", units, err)
	}
}

func TestIssue72MalformedQuotaKeyKnownOtherOwnerIsExcluded(t *testing.T) {
	db := testDB(t)
	hour := boundScanTime.Truncate(time.Hour)
	putQuotaAggRaw(t, db, hour.Format(hourLayout)+"|alice|target", []byte(`{"requests":1}`))
	from, to := hour, hour.Add(time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: "bob", From: &from, To: &to}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Fatalf("unrelated owner blocked: %v", err)
	}
	filter.ClientName = "alice"
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("affected owner reported complete: %v", err)
	}
}

func TestIssue72MalformedQuotaUnknownHourCannotHide(t *testing.T) {
	db := testDB(t)
	putQuotaAggRaw(t, db, "not-an-hour|alice|target|model", []byte(`{"requests":1}`))
	from, to := boundScanTime, boundScanTime.Add(time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), filter, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("unknown hour was skipped: %v", err)
	}
	filter.ClientName = "bob"
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Fatalf("known other owner was blocked: %v", err)
	}
}

func TestIssue72InterruptedBackfillRetriesWithoutFalseEvidence(t *testing.T) {
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		t.Run(source, func(t *testing.T) {
			db := testDB(t)
			event := fixedPlainEvent()
			event.InputTokens = 1
			if err := db.Update(func(tx *bolt.Tx) error {
				for i := 0; i < usageBackfillBatchSize+1; i++ {
					event.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", i)
					data, err := json.Marshal(event)
					if err != nil {
						return err
					}
					if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), data); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if source == BucketUsageAggHourly {
				if err := BackfillQuotaUsageAgg(db); err != nil {
					t.Fatal(err)
				}
			} else if err := BackfillUsageAgg(db); err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEvents)).ForEach(func(_, raw []byte) error {
					var row usage.Event
					if err := json.Unmarshal(raw, &row); err != nil {
						return err
					}
					if source == BucketUsageAggHourly {
						return bumpAgg(tx, row)
					}
					return bumpQuotaAgg(tx, row)
				})
			}); err != nil {
				t.Fatal(err)
			}
			fault := errors.New("injected crash after first batch")
			hook := func(processed int64) error {
				if processed == usageBackfillBatchSize {
					return fault
				}
				return nil
			}
			var err error
			if source == BucketUsageAggHourly {
				err = backfillUsageAgg(db, hook)
			} else {
				err = backfillQuotaUsageAgg(db, hook)
			}
			if !errors.Is(err, fault) {
				t.Fatalf("first batch did not reach injected crash: %v", err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				key := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
				if source == BucketQuotaUsageAggHourly {
					key = quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				}
				var cell AggCell
				if err := json.Unmarshal(tx.Bucket([]byte(source)).Get(key), &cell); err != nil {
					return err
				}
				if cell.InputTokens != usageBackfillBatchSize+1 {
					t.Errorf("interruption changed original source: %+v", cell)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
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
			if source == BucketUsageAggHourly {
				err = BackfillUsageAgg(db)
			} else {
				err = BackfillQuotaUsageAgg(db)
			}
			if err != nil {
				t.Fatal(err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			units, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if err != nil || len(units) != 1 || units[0].InputTokens != usageBackfillBatchSize+1 {
				t.Fatalf("retry produced false evidence or incorrect usage: units=%v err=%v", units, err)
			}
		})
	}
}

func TestIssue72BackfillRejectsConcurrentChangesAndCanRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *bolt.DB, *UsageStore, usage.Event)
	}{
		{"raw_and_source", func(t *testing.T, db *bolt.DB, store *UsageStore, event usage.Event) {
			event.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaad11"
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
		}},
		{"source_only", func(t *testing.T, db *bolt.DB, store *UsageStore, event usage.Event) {
			key := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
			putAggRaw(t, db, string(key), []byte(`{"requests":3,"input_tokens":9}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			called := false
			err := backfillUsageAgg(db, func(_ int64) error {
				if !called {
					called = true
					tc.mutate(t, db, store, event)
				}
				return nil
			})
			if err == nil || !called {
				t.Fatalf("concurrent change was accepted: %v", err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageAggBuiltMetaKey)) != nil {
					t.Error("failed rebuild marked complete")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := BackfillUsageAgg(db); err != nil {
				t.Fatalf("retry: %v", err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			_, err = store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
			if tc.name == "source_only" && !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("changed source was discarded without evidence: %v", err)
			}
			if tc.name == "raw_and_source" && err != nil {
				t.Fatalf("matching concurrent record became incomplete: %v", err)
			}
		})
	}
}

func TestIssue72BackfillRejectsUnownedStaging(t *testing.T) {
	db := testDB(t)
	stage := []byte(usageBackfillStagePrefix + BucketUsageAggHourly)
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket(stage)
		if err != nil {
			return err
		}
		return b.Put([]byte("original"), []byte("evidence"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillUsageAgg(db); err == nil {
		t.Fatal("unowned stage was accepted")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket(stage).Get([]byte("original")); string(got) != "evidence" {
			t.Errorf("unowned stage changed: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue72ConcurrentBackfillDoesNotMixStages(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	called := false
	err := backfillUsageAgg(db, func(_ int64) error {
		if !called {
			called = true
			return BackfillUsageAgg(db)
		}
		return nil
	})
	if err == nil || !called {
		t.Fatalf("superseded rebuild was accepted: %v", err)
	}
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("concurrent rebuild changed accounting: %v %v", units, err)
	}
}

func TestIssue72UnknownEvidenceUsesKnownDimensions(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		at          time.Time
		queryOwner  string
		queryAt     time.Time
		incomplete  bool
	}{
		{"known_owner_other_client", "alice", time.Time{}, "bob", boundScanTime, false},
		{"known_owner_same_client", "alice", time.Time{}, "alice", boundScanTime, true},
		{"known_time_other_hour", "", boundScanTime, "bob", boundScanTime.Add(48 * time.Hour), false},
		{"known_time_same_hour", "", boundScanTime, "bob", boundScanTime, true},
		{"both_unknown", "", time.Time{}, "bob", boundScanTime, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			e := fixedPlainEvent()
			e.ClientName, e.Timestamp = tc.owner, tc.at
			if err := store.RegisterUnresolvedUsage(e, []byte(tc.name)); err != nil {
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
			from, to := tc.queryAt.Truncate(time.Hour), tc.queryAt.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			_, err = clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: tc.queryOwner, From: &from, To: &to}, replayCosts, false)
			if tc.incomplete && (!errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing)) {
				t.Fatalf("unknown dimension not reported: %v", err)
			}
			if !tc.incomplete && err != nil {
				t.Fatalf("known disjoint dimension blocked query: %v", err)
			}
		})
	}
}

func TestIssue72CrossClientIdentityConflictRemainsIncomplete(t *testing.T) {
	for _, beforeCommit := range []bool{true, false} {
		t.Run(map[bool]string{true: "before_commit", false: "after_commit"}[beforeCommit], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			alice := fixedPlainEvent()
			bob := alice
			bob.ClientName = "bob"
			if beforeCommit {
				if err := store.RegisterUnresolvedUsage(bob, []byte("bob original")); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Record(alice); err != nil {
				t.Fatal(err)
			}
			if !beforeCommit {
				if err := store.RegisterUnresolvedUsage(bob, []byte("bob original")); err != nil {
					t.Fatal(err)
				}
			}
			from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("bob conflict was incorrectly resolved: %v", err)
			}
			if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false); err != nil {
				t.Fatalf("alice incorrectly blocked: %v", err)
			}
		})
	}
}

func TestIssue72CrossClientConflictWithPrunedReceiptIsUnverified(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(alice.Timestamp.Add(time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterUnresolvedUsage(bob, []byte("bob original")); err != nil {
		t.Fatal(err)
	}
	from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("receipt-only conflict guessed owner: %v", err)
	}
}

func TestIssue72MigratesPreviouslyMisresolvedCrossClientEvidence(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	bob := alice
	bob.ClientName = "bob"
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	item := unresolvedUsage{Event: bob, Original: []byte("bob original"), Resolved: true}
	value, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := usage.RetryEquivalentDigest(bob)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(value)
	key := append([]byte("r|"+usageKey(bob.Timestamp, bob.RecordID)+"|"+digest+"|"), sum[:]...)
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte(unresolvedBucket))
		if err != nil {
			return err
		}
		if err := b.Put(key, value); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
			return err
		}
		return meta.Put([]byte("usage_unresolved_events_canonical_v3"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
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
	from, to := bob.Timestamp.Truncate(time.Hour), bob.Timestamp.Add(time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "bob", From: &from, To: &to}, replayCosts, false); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("previously misresolved evidence was not restored: %v", err)
	}
}

func TestIssue72LegacyResolvedAndUnresolvedEvidenceMergeOnRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(event)
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte(unresolvedBucket))
		if err != nil {
			return err
		}
		for _, resolved := range []bool{false, true} {
			item := unresolvedUsage{Event: event, Original: original, Resolved: resolved}
			value, err := json.Marshal(item)
			if err != nil {
				return err
			}
			digest, err := usage.RetryEquivalentDigest(event)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(value)
			key := append([]byte("r|"+usageKey(event.Timestamp, event.RecordID)+"|"+digest+"|"), sum[:]...)
			if err := bucket.Put(key, value); err != nil {
				return err
			}
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
			return err
		}
		return meta.Put([]byte("usage_unresolved_events_canonical_v3"), []byte("1"))
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
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		count := 0
		err := tx.Bucket([]byte(unresolvedBucket)).ForEach(func(_, raw []byte) error {
			count++
			var item unresolvedUsage
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if !item.Resolved || !bytes.Equal(item.Original, original) {
				t.Errorf("bad migrated evidence: %+v", item)
			}
			return nil
		})
		if count != 1 {
			t.Errorf("evidence duplicated after restart: %d", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("resolved evidence revived: %v", err)
	}
}

func TestIssue72UnresolvedEvidenceIdentityIgnoresResolvedStatus(t *testing.T) {
	event := fixedPlainEvent()
	original, _ := json.Marshal(event)
	unresolved := unresolvedUsage{Event: event, Original: original}
	resolved := unresolved
	resolved.Resolved = true
	a, _, err := unresolvedEvidenceKey(unresolved)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := unresolvedEvidenceKey(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("resolution changed evidence identity: %x != %x", a, b)
	}
	db := testDB(t)
	store := clockedStore(db)
	if err := store.RegisterUnresolvedUsage(event, original); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return putUnresolvedUsage(tx, event, original) }); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	if _, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("resolved evidence revived: %v", err)
	}
}
