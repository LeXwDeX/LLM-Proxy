package nosql

import (
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A report holds a read transaction for seconds. A bbolt remap waits for every
// in-flight reader and blocks all new transactions meanwhile, so database growth
// during a report used to stall writers and new readers for the report's
// duration. OpenDB reserves the mapping up front so ordinary growth never remaps.
func TestGrowthDuringLongReadDoesNotBlockTransactions(t *testing.T) {
	if !mmapReservationSupported {
		t.Skip("mmap reservation disabled on this platform")
	}
	db, err := OpenDB(filepath.Join(t.TempDir(), "stall.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	release := make(chan struct{})
	started := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_ = db.View(func(tx *bolt.Tx) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	released := false
	releaseReader := func() {
		if !released {
			released = true
			close(release)
			<-readerDone
		}
	}
	defer releaseReader()

	const limit = 2 * time.Second
	growth := make(chan time.Duration, 1)
	go func() {
		st := time.Now()
		// 8 MiB comfortably exceeds any default initial mapping of a fresh file.
		err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketMeta)).Put([]byte("growth"), make([]byte, 8<<20))
		})
		if err != nil {
			t.Errorf("growth update: %v", err)
		}
		growth <- time.Since(st)
	}()

	select {
	case d := <-growth:
		if d > limit {
			t.Fatalf("growth commit took %v with a report in flight, want < %v", d, limit)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("growth commit still blocked after 6s behind an in-flight read transaction")
	}

	st := time.Now()
	if err := db.View(func(tx *bolt.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(st); d > limit {
		t.Fatalf("new read transaction took %v during report, want < %v", d, limit)
	}
	st = time.Now()
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(BucketMeta)).Put([]byte("probe"), []byte("x")) }); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(st); d > limit {
		t.Fatalf("small write took %v during report, want < %v", d, limit)
	}
	releaseReader()
}
