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

func TestOwnerP1MissingRawAndOwnerRetainsIndexIdentity(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(rawOwnerBucket)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	before := issue72ReadTransactionID(t, db)
	if exists, err := store.RecordExists(event); exists || err == nil {
		t.Errorf("missing identity accepted: %v %v", exists, err)
	}
	if after := issue72ReadTransactionID(t, db); before != after {
		t.Error("lookup wrote")
	}
	if added, err := store.RecordOnce(event); added || err == nil {
		t.Errorf("missing identity added: %v %v", added, err)
	}
}

func TestOwnerP1OrphanBlocksDirectAccounting(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("orphan costs: %v %v", units, err)
	}
	if cells, err := store.SumQuotaByClientRange(event.ClientName, from, to); len(cells) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("orphan quota: %v %v", cells, err)
	}
	filter.ClientName = "bob"
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Errorf("other client: %v", err)
	}
}

func TestOwnerP1OrphanCannotHideBehindUnrelatedCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	other := event
	other.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	other.Model = "other"
	if err := store.Record(other); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete([]byte(usageKey(event.Timestamp, event.RecordID))); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("orphan hidden: %v %v", units, err)
	}
}

func TestOwnerP1CorruptPayloadCannotExcludeOriginalModel(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	data := []byte(fmt.Sprintf(`{"client_name":%q,"timestamp":%q,"model":"other","input_tokens":"bad"}`, event.ClientName, event.Timestamp.Format(time.RFC3339Nano)))
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(key, data) }); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, Model: event.Model, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("original model: %v %v", units, err)
	}
	filter.ClientName = "bob"
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Errorf("other client: %v", err)
	}
}

func TestOwnerP1QuarantineScopePersistsAfterRestart(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = "bob"
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	bad := []byte("{broken")
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(key, bad) }); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
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
	if err := db.View(func(tx *bolt.Tx) error {
		var q usageQuarantineRecord
		if err := json.Unmarshal(tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get(key), &q); err != nil {
			return err
		}
		if !bytes.Equal(q.RawValue, bad) {
			t.Error("original bytes lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store = clockedStore(db)
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	if _, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); err != nil {
		t.Errorf("alice blocked: %v", err)
	}
	filter.ClientName = "bob"
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Errorf("bob not blocked: %v %v", units, err)
	}
}

func TestOwnerP1QuarantineLostOwnerIsNotExcluded(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	event.ClientName = "bob"
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken")) }); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(rawOwnerBucket)).Delete(key) }); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false)
	if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("missing quarantine owner excluded: %v %v", units, err)
	}
}

func TestOwnerP1BundleMissingRawAndOwnerCannotBeReused(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedIdentityBundle()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil || len(rows) < 2 {
		t.Fatalf("fixture: %v %v", rows, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := []byte(keys[1])
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(rawOwnerBucket)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.RecordExists(event); exists || err == nil {
		t.Fatalf("partial lookup: %v %v", exists, err)
	}
	if added, err := store.RecordOnce(event); added || err == nil {
		t.Fatalf("partial write: %v %v", added, err)
	}
}
