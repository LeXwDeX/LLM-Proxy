package nosql

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

func TestAsyncRequestStatsCloseWritesAllEvents(t *testing.T) {
	db := testDB(t)
	store := NewRequestStatsStore(db)
	recorder := NewAsyncRequestStatsRecorder(store, AsyncRequestStatsOptions{QueueSize: 4096, MaxBatch: 32}, nil)
	now := time.Now().UTC()
	outcomes := []requeststats.Outcome{requeststats.Success, requeststats.Failure, requeststats.Canceled}
	const n = 600
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			at := now.Add(-time.Duration(i%3) * time.Minute)
			if err := recorder.Record(requeststats.Event{Timestamp: at, Outcome: outcomes[i%3]}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stats, err := store.Snapshot(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != n || stats.Success != n/3 || stats.Failure != n/3 || stats.Canceled != n/3 {
		t.Fatalf("stats after Close = %+v, want %d split evenly", stats.Counters, n)
	}
	if recorder.Failures() != 0 {
		t.Fatalf("unexpected failures %d", recorder.Failures())
	}
	if err := recorder.Record(requeststats.Event{Timestamp: now, Outcome: requeststats.Success}); !errors.Is(err, ErrRequestStatsClosed) {
		t.Fatalf("Record after Close = %v, want ErrRequestStatsClosed", err)
	}
}

func TestAsyncRequestStatsFullQueueDropsWithoutBlocking(t *testing.T) {
	db := testDB(t)
	store := NewRequestStatsStore(db)
	recorder := NewAsyncRequestStatsRecorder(store, AsyncRequestStatsOptions{QueueSize: 2, MaxBatch: 1}, nil)
	// Hold the database write lock so the background writer stalls.
	tx, err := db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	accepted, dropped := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			err := recorder.Record(requeststats.Event{Timestamp: now, Outcome: requeststats.Success})
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrRequestStatsQueueFull):
				dropped++
			default:
				t.Errorf("Record: %v", err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full stats queue blocked Record")
	}
	if dropped == 0 || accepted == 0 {
		t.Fatalf("accepted=%d dropped=%d, want both non-zero", accepted, dropped)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Snapshot(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if int(stats.Total) != accepted {
		t.Fatalf("stored=%d accepted=%d", stats.Total, accepted)
	}
}

func TestAsyncRequestStatsRejectsInvalidEventsSynchronously(t *testing.T) {
	db := testDB(t)
	recorder := NewAsyncRequestStatsRecorder(NewRequestStatsStore(db), AsyncRequestStatsOptions{}, nil)
	defer recorder.Close()
	if err := recorder.Record(requeststats.Event{Outcome: requeststats.Success}); err == nil {
		t.Fatal("zero timestamp accepted")
	}
	if err := recorder.Record(requeststats.Event{Timestamp: time.Now(), Outcome: "bogus"}); err == nil {
		t.Fatal("unknown outcome accepted")
	}
}

// A corrupt minute cell must only lose that minute's events; the rest of the
// batch is still written and the loss is reported as record failures.
func TestAsyncRequestStatsBatchIsolatesCorruptMinute(t *testing.T) {
	db := testDB(t)
	store := NewRequestStatsStore(db)
	now := time.Now().UTC().Truncate(time.Minute).Add(10 * time.Second)
	bad := now.Add(-time.Minute)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketRequestStats)).Put(requestMinuteKey(bad), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	events := []requeststats.Event{
		{Timestamp: now, Outcome: requeststats.Success},
		{Timestamp: bad, Outcome: requeststats.Success},
		{Timestamp: now, Outcome: requeststats.Failure},
	}
	failed, err := store.RecordBatch(events)
	if err == nil || failed != 1 {
		t.Fatalf("RecordBatch failed=%d err=%v, want 1 failure", failed, err)
	}
	var cell []byte
	if err := db.View(func(tx *bolt.Tx) error {
		cell = append([]byte(nil), tx.Bucket([]byte(BucketRequestStats)).Get(requestMinuteKey(now))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	counters, err := decodeRequestCounters(cell)
	if err != nil || counters.Total != 2 || counters.Success != 1 || counters.Failure != 1 {
		t.Fatalf("healthy minute counters=%+v err=%v", counters, err)
	}

	recorder := NewAsyncRequestStatsRecorder(store, AsyncRequestStatsOptions{}, nil)
	if err := recorder.Record(requeststats.Event{Timestamp: bad, Outcome: requeststats.Success}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if recorder.Failures() != 1 {
		t.Fatalf("async write failure not reported: %d", recorder.Failures())
	}
}
