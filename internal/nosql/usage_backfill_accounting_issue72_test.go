package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72BackfillDetectsEqualCountDifferentAccounting(t *testing.T) {
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		t.Run(source, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			key := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
			if source == BucketQuotaUsageAggHourly {
				key = quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(source))
				var cell AggCell
				if err := json.Unmarshal(bucket.Get(key), &cell); err != nil {
					return err
				}
				cell.InputTokens++
				data, err := json.Marshal(cell)
				if err != nil {
					return err
				}
				return bucket.Put(key, data)
			}); err != nil {
				t.Fatal(err)
			}
			if source == BucketUsageAggHourly {
				if err := BackfillUsageAgg(db); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := BackfillQuotaUsageAgg(db); err != nil {
					t.Fatal(err)
				}
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("same count hid changed accounting: units=%v err=%v", units, err)
			}
			filter.ClientName = "bob"
			if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
				t.Fatalf("unrelated client blocked: %v", err)
			}
		})
	}
}

func TestIssue72BackfillEvidenceSurvivesRebuildAndRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, event)
	oldWatermarkDone(t, db, time.Time{})
	key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	var original []byte
	if err := db.View(func(tx *bolt.Tx) error {
		original = append([]byte(nil), tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(key)...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := BackfillQuotaUsageAgg(db); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketMeta)).Delete([]byte(quotaUsageAggBuiltMetaKey))
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if values := issue72SnapshotValues(t, db, BucketQuotaUsageAggHourly, key); !issue72HasBytes(values, original) {
		t.Fatal("original quota evidence was lost")
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
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("lost evidence after restart: %v %v", units, err)
	}
	if added, err := store.RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("lost identity accepted: added=%v err=%v", added, err)
	}
}

func TestIssue72BackfillThenNormalPruningKeepsCompleteAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(event.Timestamp.Add(time.Second), 100); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if err != nil || costUnitTotal(units) != 2 {
		t.Fatalf("normal pruning lost valid aggregate: units=%v err=%v", units, err)
	}
}

func TestIssue72BackfillUnknownOwnerBlocksActivation(t *testing.T) {
	db := testDB(t)
	key := []byte("2026-10-21T09|ambiguous|alice|part|model|target")
	putAggRaw(t, db, string(key), []byte(`{"requests":1,"input_tokens":5}`))
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour - time.Nanosecond)
	_, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "unrelated", From: &from, To: &to}, replayCosts, false)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) {
		t.Fatalf("ambiguous owner was guessed: %v", err)
	}
	from, to = from.Add(24*time.Hour), to.Add(24*time.Hour)
	if _, err := clockedStore(db).ReadCostUnits(context.Background(), usage.Filter{ClientName: "unrelated", From: &from, To: &to}, replayCosts, false); err != nil {
		t.Fatalf("known unrelated hour was blocked: %v", err)
	}
}

func TestIssue72BackfillDoesNotTreatOverlappingLegacyAndV2AsOneCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketUsageAggHourly))
		v2 := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
		return bucket.Put(legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target), bucket.Get(v2))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	_, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false)
	if !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("overlapping source cells were treated as one: %v", err)
	}
}

func TestIssue72BackfillEvidenceFailureRollsBackReset(t *testing.T) {
	db := testDB(t)
	event := fixedPlainEvent()
	key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	value := []byte(`{"requests":1,"input_tokens":5}`)
	putQuotaAggRaw(t, db, string(key), value)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedInitialized), []byte("invalid"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err == nil {
		t.Fatal("backfill ignored evidence failure")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(key)) != string(value) {
			t.Error("failed evidence write erased source")
		}
		if tx.Bucket([]byte(usageBackfillSnapshotBucket)) != nil {
			t.Error("failed transaction committed a snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
