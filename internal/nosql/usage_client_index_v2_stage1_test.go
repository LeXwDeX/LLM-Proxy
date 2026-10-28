package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func stopIndexV2AfterOne(t *testing.T, db *bolt.DB) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("first batch: %v", err)
	}
}

func TestIndexV2BatchPersistsBoundProgressAndProof(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("first batch: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		var progress struct {
			Version    int    `json:"version"`
			Generation string `json:"generation"`
			Phase      string `json:"phase"`
			Through    []byte `json:"through"`
			Upper      []byte `json:"upper"`
			Binding    string `json:"binding"`
		}
		if json.Unmarshal(meta.Get([]byte("usage_client_index_progress_v2")), &progress) != nil || progress.Version != 2 || progress.Generation == "" || progress.Phase != "scan" || progress.Binding == "" || !bytes.Equal(progress.Through, meta.Get([]byte(usageClientIndexCursor))) || bytes.Compare(progress.Through, progress.Upper) > 0 {
			t.Fatalf("invalid v2 progress: %+v", progress)
		}
		proofs := tx.Bucket([]byte("usage_client_index_proofs_v2"))
		if proofs == nil || proofs.Get(progress.Through) == nil {
			t.Fatal("committed batch lacks raw-key proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2PartialSourceLossBlocksResume(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("usage_client_index_proofs_v2")) }); err != nil {
		t.Fatal(err)
	}
	before := indexBucketSnapshot(t, db, BucketMeta)
	if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("missing proof bucket accepted: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("usage_client_index_proofs_v2")) != nil || !bytes.Equal([]byte(before["usage_client_index_progress_v2"]), tx.Bucket([]byte(BucketMeta)).Get([]byte("usage_client_index_progress_v2"))) {
			t.Fatal("lost proof source was replaced or progress changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2CorruptOrMissingAnchorsRejectResume(t *testing.T) {
	for _, damage := range []string{"progress-missing", "required-missing", "progress-corrupt", "required-corrupt", "cursor-forward", "cursor-missing", "source-loss"} {
		t.Run(damage, func(t *testing.T) {
			db, _, _, _ := cursorCostFixture(t)
			stopIndexV2AfterOne(t, db)
			if err := db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				switch damage {
				case "progress-missing":
					return meta.Delete([]byte(indexProgressV2))
				case "required-missing":
					return meta.Delete([]byte(indexRequiredV2))
				case "progress-corrupt":
					return meta.Put([]byte(indexProgressV2), []byte("{}"))
				case "required-corrupt":
					return meta.Put([]byte(indexRequiredV2), []byte("bad"))
				case "cursor-forward":
					c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
					c.First()
					k, _ := c.Next()
					return meta.Put([]byte(usageClientIndexCursor), k)
				case "cursor-missing":
					return meta.Delete([]byte(usageClientIndexCursor))
				default:
					return meta.Put([]byte(usageClientIndexSourceMissing), []byte("1"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			before := indexBucketSnapshot(t, db, BucketMeta)
			if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
				t.Fatalf("%s accepted: %v", damage, err)
			}
			if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketMeta)) {
				t.Fatal("failed resume modified metadata")
			}
		})
	}
}

func TestIndexV2AtomicProofAndRollback(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	stopIndexV2AfterOne(t, db)
	var key, data []byte
	if err := db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		c.First()
		k, v := c.Next()
		key, data = bytes.Clone(k), bytes.Clone(v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := indexBucketSnapshot(t, db, indexProofsV2)
	indexBefore := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := ReindexUsageEvent(tx, key, data, data); err != nil {
			return err
		}
		return errors.New("injected rollback")
	}); err == nil {
		t.Fatal("rollback fixture unexpectedly committed")
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, indexProofsV2)) || !reflect.DeepEqual(indexBefore, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) {
		t.Fatal("reindex or proof changed after rollback")
	}
	if err := db.Update(func(tx *bolt.Tx) error { return ReindexUsageEvent(tx, key, data, data) }); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		var proof indexProof
		if json.Unmarshal(tx.Bucket([]byte(indexProofsV2)).Get(key), &proof) != nil || proof.Origin != "atomic" || proof.Binding != proof.binding(key) {
			t.Fatalf("invalid atomic proof: %+v", proof)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before = indexBucketSnapshot(t, db, indexProofsV2)
	if err := MigrateUsageClientIndex(context.Background(), db, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, indexProofsV2)) {
		t.Fatal("scan overwrote an atomic proof")
	}
}

func TestIndexV2ResumeAfterThroughPruned(t *testing.T) {
	db, store, _, day := cursorCostFixture(t)
	stopIndexV2AfterOne(t, db)
	if n, err := store.PruneEventsBefore(day.Add(9*time.Hour+30*time.Minute), 1); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := MigrateUsageClientIndex(context.Background(), db, 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := indexBucketSnapshot(t, db, indexProofsV2); len(got) != 2 {
		t.Fatalf("prune erased proof: %d", len(got))
	}
}

func TestIndexV2LegacyPartialCannotBeCertified(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		key, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexCursor), key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("v1 partial accepted: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(indexProofsV2)) != nil || tx.Bucket([]byte(BucketMeta)).Get([]byte(indexProgressV2)) != nil {
			t.Fatal("invented v2 evidence for v1 partial")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2InvalidExistingProofRejectsReindex(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	stopIndexV2AfterOne(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		key, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return tx.Bucket([]byte(indexProofsV2)).Put(key, []byte("{}"))
	}); err != nil {
		t.Fatal(err)
	}
	before := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	if err := db.Update(func(tx *bolt.Tx) error {
		key, data := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return ReindexUsageEvent(tx, key, data, data)
	}); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("corrupt proof accepted: %v", err)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) {
		t.Fatal("failed reindex changed index")
	}
}

func TestIndexV2FailedFirstBatchRollsBackInitialization(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		c := b.Cursor()
		c.First()
		key, _ := c.Next()
		return b.Put(bytes.Clone(key), []byte("bad json"))
	}); err != nil {
		t.Fatal(err)
	}
	meta := indexBucketSnapshot(t, db, BucketMeta)
	index := indexBucketSnapshot(t, db, BucketUsageEventsByClient)
	hours := indexBucketSnapshot(t, db, BucketUsageClientHours)
	if err := MigrateUsageClientIndex(context.Background(), db, 2); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("bad batch: %v", err)
	}
	if !reflect.DeepEqual(meta, indexBucketSnapshot(t, db, BucketMeta)) || !reflect.DeepEqual(index, indexBucketSnapshot(t, db, BucketUsageEventsByClient)) || !reflect.DeepEqual(hours, indexBucketSnapshot(t, db, BucketUsageClientHours)) {
		t.Fatal("failed batch left partial writes")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(indexProofsV2)) != nil {
			t.Fatal("failed batch created proof bucket")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2ResumeAfterReopen(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	stopIndexV2AfterOne(t, db)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := MigrateUsageClientIndex(context.Background(), reopened, 1); err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(func(tx *bolt.Tx) error {
		p, err := loadIndexProgress(tx)
		if err != nil || p == nil || p.Phase != "complete" {
			t.Fatalf("resume progress: %+v %v", p, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
