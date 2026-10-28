package nosql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIssue72MissingQuotaSourceReportsActivationBlocker(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketQuotaUsageAggHourly)) }); err != nil {
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
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Truncate(time.Hour).Add(time.Hour)
	for _, client := range []string{event.ClientName, "unrelated"} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, replayCosts, false)
		if len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) || !errors.Is(err, ErrUsageAggregateSourceMissing) {
			t.Errorf("client %q: units=%v err=%v", client, units, err)
		}
	}
	if added, err := store.RecordOnce(event); added || err != nil {
		t.Errorf("verified identity changed: added=%v err=%v", added, err)
	}
}

func TestIssue72BackfillRetainsLostIdentityBoundaryAcrossRestart(t *testing.T) {
	db := testDB(t)
	event := fixedPlainEvent()
	if err := clockedStore(db).Record(event); err != nil {
		t.Fatal(err)
	}
	deleteUsageRawAndIndex(t, db, event)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageAggHourly)).Delete(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))
	}); err != nil {
		t.Fatal(err)
	}
	oldWatermarkDone(t, db, time.Time{})
	if err := BackfillQuotaUsageAgg(db); err != nil {
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
	if added, err := clockedStore(db).RecordOnce(event); added || !errors.Is(err, ErrUsageIdentityUnverified) {
		t.Fatalf("lost identity admitted after restart: added=%v err=%v", added, err)
	}
}
