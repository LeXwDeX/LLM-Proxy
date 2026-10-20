package nosql

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

func TestRequestStatsBoundariesConcurrentAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewRequestStatsStore(db)
	now := time.Now().UTC().Truncate(time.Minute).Add(45 * time.Second)
	before, err := store.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if before.Total != 0 || before.SuccessRate != nil || before.Since.IsZero() {
		t.Fatalf("empty=%+v", before)
	}
	for _, event := range []requeststats.Event{
		{Timestamp: before.From.Add(-time.Minute), Outcome: requeststats.Success},
		{Timestamp: before.From, Outcome: requeststats.Failure},
		{Timestamp: before.To.Add(-time.Nanosecond), Outcome: requeststats.Canceled},
		{Timestamp: before.To, Outcome: requeststats.Success},
	} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Record(requeststats.Event{Timestamp: before.From.Add(time.Hour), Outcome: requeststats.Success}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	stats, err := store.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 62 || stats.Success != 60 || stats.Failure != 1 || stats.Canceled != 1 || stats.SuccessRate == nil || *stats.SuccessRate != float64(60)/62*100 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.From != before.From || stats.To != before.To {
		t.Fatalf("window=%+v", stats)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stats, err = NewRequestStatsStore(db).Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Since.Equal(before.Since) || stats.Total != 62 {
		t.Fatalf("reopen=%+v", stats)
	}
}

func TestRequestStatsRetentionAndFailures(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewRequestStatsStore(db)
	now := time.Now().UTC().Truncate(time.Minute)
	cutoff := now.Add(-requeststats.Retention)
	for _, at := range []time.Time{cutoff.Add(-2 * time.Minute), cutoff.Add(-time.Minute), cutoff, now.Add(-time.Minute)} {
		if err := store.Record(requeststats.Event{Timestamp: at, Outcome: requeststats.Success}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int{1, 1, 0} {
		n, err := store.PruneBefore(cutoff, 1)
		if err != nil || n != want {
			t.Fatalf("prune=%d %v want %d", n, err, want)
		}
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketRequestStats)).Get(requestMinuteKey(cutoff)) == nil {
			t.Error("inclusive retention boundary removed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(requeststats.Event{Timestamp: now, Outcome: "invalid"}); err == nil {
		t.Fatal("invalid outcome accepted")
	}
	if _, err := store.Snapshot(time.Time{}); err == nil {
		t.Fatal("invalid query accepted")
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketRequestStats)).Put(requestMinuteKey(now.Add(-time.Minute)), []byte(`{"total":1,"success":2}`))
	}); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.Snapshot(now); err == nil || stats.Available {
		t.Fatalf("corrupt data hidden: %+v %v", stats, err)
	}
	if err := store.Record(requeststats.Event{Timestamp: now.Add(-time.Minute), Outcome: requeststats.Success}); err == nil {
		t.Fatal("corrupt write accepted")
	}
	db.Close()
	if err := store.Record(requeststats.Event{Timestamp: now, Outcome: requeststats.Success}); err == nil {
		t.Fatal("closed db write hidden")
	}
	if stats, err := store.Snapshot(now); err == nil || stats.Available {
		t.Fatalf("closed db query hidden: %+v %v", stats, err)
	}
}

func TestRequestStatsRejectsMalformedCellsAndMetadata(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewRequestStatsStore(db)
	now := time.Now().UTC()
	key := requestMinuteKey(now.Add(-time.Minute))
	for _, raw := range []string{"null", "{}", `{"total":1,"success":1}`, `{"total":0,"success":0,"failure":0,"canceled":1}`, `{"total":-1}`} {
		if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketRequestStats)).Put(key, []byte(raw)) }); err != nil {
			t.Fatal(err)
		}
		if stats, err := store.Snapshot(now); err == nil || stats.Available {
			t.Errorf("malformed %q accepted: %+v %v", raw, stats, err)
		}
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketRequestStats)).Delete(key); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketMeta)).Put(requestStatsSinceKey, []byte("broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.Snapshot(now); err == nil || stats.Available {
		t.Fatalf("invalid metadata accepted: %+v %v", stats, err)
	}
}
