package nosql

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	bolt "go.etcd.io/bbolt"
)

const issue72SnapshotBucket = "usage_aggregate_backfill_snapshots_v1"

func issue72SnapshotValues(t *testing.T, db *bolt.DB, source string, key []byte) [][]byte {
	t.Helper()
	var values [][]byte
	if err := db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket([]byte(issue72SnapshotBucket))
		if root == nil {
			return nil
		}
		generations := root.Bucket([]byte(source))
		if generations == nil {
			return nil
		}
		return generations.ForEach(func(k, v []byte) error {
			if v != nil {
				return fmt.Errorf("snapshot generation is not a bucket")
			}
			if value := generations.Bucket(k).Get(key); value != nil {
				values = append(values, append([]byte(nil), value...))
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

func issue72HasBytes(values [][]byte, want []byte) bool {
	for _, value := range values {
		if bytes.Equal(value, want) {
			return true
		}
	}
	return false
}

func TestIssue72BackfillPreservesOriginalAggregateBytes(t *testing.T) {
	db := testDB(t)
	event := fixedPlainEvent()
	if err := NewUsageStore(db).Record(event); err != nil {
		t.Fatal(err)
	}
	usageKey := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
	quotaKey := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	usageBytes := []byte("{corrupt usage \xff")
	quotaBytes := []byte("{corrupt quota \xfe")
	putAggRaw(t, db, string(usageKey), usageBytes)
	putQuotaAggRaw(t, db, string(quotaKey), quotaBytes)
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if values := issue72SnapshotValues(t, db, BucketUsageAggHourly, usageKey); !issue72HasBytes(values, usageBytes) {
		t.Errorf("usage backfill discarded original key/value: snapshots=%q", values)
	}
	if values := issue72SnapshotValues(t, db, BucketQuotaUsageAggHourly, quotaKey); !issue72HasBytes(values, quotaBytes) {
		t.Errorf("quota backfill discarded original key/value: snapshots=%q", values)
	}
}

func TestIssue72BackfillSnapshotFailureRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	event := fixedPlainEvent()
	key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	first := []byte("old untrusted quota bytes")
	putQuotaAggRaw(t, db, string(key), first)
	if err := db.Update(func(tx *bolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists([]byte(issue72SnapshotBucket))
		if err != nil {
			return err
		}
		return root.Put([]byte(BucketQuotaUsageAggHourly), []byte("conflicting value"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err == nil {
		t.Error("backfill succeeded despite failing to snapshot its source")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(key); !bytes.Equal(got, first) {
			t.Errorf("failed snapshot changed source: got %q want %q", got, first)
		}
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(quotaUsageAggBuiltMetaKey)) != nil {
			t.Error("failed snapshot marked the backfill complete")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dbPath := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(issue72SnapshotBucket)).Delete([]byte(BucketQuotaUsageAggHourly))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatalf("retry after removing the storage fault: %v", err)
	}
	if values := issue72SnapshotValues(t, db, BucketQuotaUsageAggHourly, key); !issue72HasBytes(values, first) {
		t.Errorf("retry failed to preserve original evidence: snapshots=%q", values)
	}
	second := []byte("later source bytes")
	putQuotaAggRaw(t, db, string(key), second)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Delete([]byte(quotaUsageAggBuiltMetaKey))
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	values := issue72SnapshotValues(t, db, BucketQuotaUsageAggHourly, key)
	if !issue72HasBytes(values, first) || !issue72HasBytes(values, second) {
		t.Errorf("later rebuild overwrote earlier evidence: snapshots=%q", values)
	}
}

func TestIssue72BackfillSnapshotEmptyValueAndNestedBucket(t *testing.T) {
	t.Run("empty_value", func(t *testing.T) {
		db := testDB(t)
		key := []byte("malformed-but-original")
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(key, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if err := BackfillQuotaUsageAgg(db); err != nil {
			t.Fatal(err)
		}
		if err := db.View(func(tx *bolt.Tx) error {
			root := tx.Bucket([]byte(issue72SnapshotBucket)).Bucket([]byte(BucketQuotaUsageAggHourly))
			generation, _ := root.Cursor().First()
			found, value := root.Bucket(generation).Cursor().Seek(key)
			if !bytes.Equal(found, key) || len(value) != 0 {
				t.Errorf("empty cell not preserved: key=%q value=%q", found, value)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unexpected_nested_bucket", func(t *testing.T) {
		db := testDB(t)
		if err := db.Update(func(tx *bolt.Tx) error {
			child, err := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).CreateBucket([]byte("unexpected"))
			if err != nil {
				return err
			}
			return child.Put([]byte("evidence"), []byte("original"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := BackfillQuotaUsageAgg(db); err == nil {
			t.Fatal("backfill deleted an unsupported nested source bucket")
		}
		if err := db.View(func(tx *bolt.Tx) error {
			child := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Bucket([]byte("unexpected"))
			if child == nil || string(child.Get([]byte("evidence"))) != "original" {
				t.Error("failed snapshot lost nested source evidence")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing_source", func(t *testing.T) {
		db := testDB(t)
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.DeleteBucket([]byte(BucketQuotaUsageAggHourly))
		}); err != nil {
			t.Fatal(err)
		}
		if err := BackfillQuotaUsageAgg(db); err != nil {
			t.Fatal(err)
		}
		if err := db.View(func(tx *bolt.Tx) error {
			root := tx.Bucket([]byte(issue72SnapshotBucket)).Bucket([]byte(BucketQuotaUsageAggHourly))
			generation, _ := root.Cursor().First()
			if root.Bucket(generation).Bucket([]byte("\x00source_bucket_missing")) == nil {
				t.Error("missing source bucket was recorded as an empty snapshot")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIssue72SnapshotResetTransactionRollback(t *testing.T) {
	db := testDB(t)
	key := []byte("original")
	value := []byte("untrusted evidence")
	putQuotaAggRaw(t, db, string(key), value)
	fault := errors.New("injected failure after reset")
	err := db.Update(func(tx *bolt.Tx) error {
		if err := snapshotUsageAggregation(tx, BucketQuotaUsageAggHourly); err != nil {
			return err
		}
		if err := tx.DeleteBucket([]byte(BucketQuotaUsageAggHourly)); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatalf("unexpected transaction error: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(issue72SnapshotBucket)) != nil {
			t.Error("failed transaction committed a partial snapshot")
		}
		if got := tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(key); !bytes.Equal(got, value) {
			t.Errorf("failed transaction lost source evidence: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := BackfillQuotaUsageAgg(db); err != nil {
		t.Fatal(err)
	}
	if values := issue72SnapshotValues(t, db, BucketQuotaUsageAggHourly, key); !issue72HasBytes(values, value) {
		t.Errorf("retry did not snapshot original evidence: %q", values)
	}
}
