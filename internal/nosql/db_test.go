package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestOpenDB(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Verify all buckets exist.
	err = db.View(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			b := tx.Bucket([]byte(name))
			if b == nil {
				t.Errorf("bucket %q not found", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// Low-concurrency Batch callers wait the full MaxBatchDelay before their
// fsync; the bbolt default (10ms) is too long for synchronous fallback paths.
func TestOpenDBUsesShortBatchDelay(t *testing.T) {
	db := testDB(t)
	if db.MaxBatchDelay != 2*time.Millisecond {
		t.Fatalf("MaxBatchDelay = %v, want 2ms", db.MaxBatchDelay)
	}
	if db.MaxBatchSize != 1000 {
		t.Fatalf("MaxBatchSize = %d, want bbolt default 1000", db.MaxBatchSize)
	}
}

func TestOpenDBInvalidPath(t *testing.T) {
	// A deeply nested path should still succeed because bbolt creates the file.
	path := filepath.Join(t.TempDir(), "sub", "deep", "test.db")
	_, err := OpenDB(path)
	// bbolt does not create parent directories, so this should fail.
	if err == nil {
		t.Fatalf("expected error for non-existent parent directory")
	}
}

// testDB is a helper that creates a temporary bbolt database for tests/benchmarks.
func testDB(t testing.TB) *bolt.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Losing the receipt source (an older binary's restore, or a partial copy that
// dropped the bucket) makes every receipt entry unverifiable while the meta
// markers and the odd-index bucket still claim the index is complete. OpenDB
// must drop the classification and let the next migration rebuild; re-creating
// an empty receipt bucket without invalidating the index would leave proofs
// trusting entries whose source no longer exists.
func TestOpenDBResetsProofOddIndexWhenReceiptSourceLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	if err := store.RecordBatch([]usage.Event{scaleEvent(900000, base.Add(3*time.Hour+2*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	odd := fixedPlainEvent()
	odd.RecordID = ""
	odd.ClientName = ""
	odd.Timestamp = base.Add(10 * time.Minute)
	oddData, err := json.Marshal(odd)
	if err != nil {
		t.Fatal(err)
	}
	// A non-canonical receipt gives the index a receipt entry whose source the
	// reopen below removes.
	receiptKey := []byte("legacy-receipt-open-recover_1")
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if err := raw.Put([]byte("zzz_legacy-open-recover"), oddData); err != nil {
			return err
		}
		return writeUsageReceipt(tx, receiptKey, "0f1e2d3c")
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetUsageProofOddIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not complete")
	}
	requireProofOddIndexMatches(t, db)
	// Drop the receipt source but keep the odd-index bucket and every marker.
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket([]byte(BucketUsageReceipts))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if proofOddReadyNow(t, db) {
		t.Fatal("OpenDB trusted an index whose receipt source had been lost")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketUsageProofOdd))
		if b == nil {
			return fmt.Errorf("odd-index bucket was not recreated")
		}
		if k, _ := b.Cursor().First(); k != nil {
			return fmt.Errorf("odd-index bucket was not reset: %q", k)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The next startup migration rebuilds the classification from the sources
	// that remain.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 8); err != nil {
		t.Fatal(err)
	}
	if !proofOddReadyNow(t, db) {
		t.Fatal("migration did not rebuild after the receipt source was lost")
	}
	requireProofOddIndexMatches(t, db)
}
