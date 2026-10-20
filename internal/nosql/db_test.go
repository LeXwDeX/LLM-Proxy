package nosql

import (
	"path/filepath"
	"testing"
	"time"

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
