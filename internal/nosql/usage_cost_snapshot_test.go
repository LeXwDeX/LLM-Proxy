package nosql

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIndexedCostUsesSingleSnapshotDuringRealWrite(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	// Reserve mapping space so the writer can commit while the reader is open.
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{InitialMmapSize: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store = clockedStore(db)
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Hour)
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(2*time.Hour - time.Nanosecond)
	filter := usage.Filter{ClientName: first.ClientName, From: &from, To: &to}
	writeDone := make(chan error, 1)
	timedOut := false
	hooks := costReadHooks{direct: &directIntegrityHooks{call: func(time.Time, time.Time) {
		go func() { writeDone <- store.Record(second) }()
		select {
		case err := <-writeDone:
			writeDone <- err
		case <-time.After(5 * time.Second):
			timedOut = true
		}
	}}}
	result, err := store.readCostUnits(context.Background(), filter, exactCostTable{replayCosts}, true, nil, hooks)
	writeErr := <-writeDone
	if timedOut || writeErr != nil || err != nil || len(result.units) != 1 || costUnitTotal(result.units) != 2 {
		t.Fatalf("snapshot units=%+v err=%v write=%v timedOut=%v", result.units, err, writeErr, timedOut)
	}
	result, err = store.readCostUnits(context.Background(), filter, exactCostTable{replayCosts}, true, nil, costReadHooks{})
	if err != nil || len(result.units) != 2 || costUnitTotal(result.units) != 4 {
		t.Fatalf("new snapshot units=%+v err=%v", result.units, err)
	}
}

func TestIndexedCostDiscardsEarlierHoursBeforeFallback(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Hour)
	for _, event := range []usage.Event{first, second} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEventsByClient))
		key, _ := b.Cursor().Seek([]byte(clientIndexPrefix(first.ClientName) + second.Timestamp.UTC().Truncate(time.Hour).Format(clientIndexTimeLayout)))
		if key == nil {
			t.Fatal("missing second-hour index")
		}
		return b.Put(append([]byte(nil), key...), []byte("missing raw"))
	}); err != nil {
		t.Fatal(err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(2*time.Hour - time.Nanosecond)
	filter := usage.Filter{ClientName: first.ClientName, From: &from, To: &to}
	want, err := store.readCostUnits(context.Background(), filter, exactCostTable{replayCosts}, true, nil, costReadHooks{forceLegacy: true})
	if err != nil {
		t.Fatal(err)
	}
	fallback := 0
	got, err := store.readCostUnits(context.Background(), filter, exactCostTable{replayCosts}, true, nil, costReadHooks{scan: func(index bool) {
		if !index {
			fallback++
		}
	}})
	if err != nil || fallback != 1 || len(got.units) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback=%d got=%+v want=%+v err=%v", fallback, got, want, err)
	}
}

func TestIndexedCostIntegrityFailureDoesNotFallback(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Delete(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
	}); err != nil {
		t.Fatal(err)
	}
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	index, fallback := 0, 0
	got, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{scan: func(indexed bool) {
		if indexed {
			index++
		} else {
			fallback++
		}
	}})
	if !errors.Is(err, ErrUsageHistoryIncomplete) || index != 0 || fallback != 0 || len(got.units) != 0 {
		t.Fatalf("result=%+v err=%v index=%d fallback=%d", got, err, index, fallback)
	}
}

func TestIndexedCostCancellationReturnsNoPartialResult(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	migrateClientIndex(t, db)
	from := event.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fallback := 0
	result, err := store.readCostUnits(ctx, usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, costReadHooks{
		decoded: cancel,
		scan: func(index bool) {
			if !index {
				fallback++
			}
		},
	})
	if !errors.Is(err, context.Canceled) || len(result.units) != 0 || len(result.raw) != 0 || fallback != 0 {
		t.Fatalf("result=%+v err=%v fallback=%d", result, err, fallback)
	}
}
