package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// MigrateUsageClientIndex builds only derived data. Raw keys, events and both
// rollups are preserved. A committed cursor resumes interrupted builds; each
// insertion is idempotent. Verification always starts over after a restart.
//
// Startup runs this in the background. Concurrent Record/prune are safe:
// they maintain the index atomically. Verification checks each client/hour in
// one snapshot, so unrelated writes do not invalidate the whole pass. Verified
// segments stay valid under the transactional Record/prune/reindex contract.
func MigrateUsageClientIndex(ctx context.Context, db *bolt.DB, batchSize int) error {
	return migrateUsageClientIndex(ctx, db, batchSize, nil)
}

func migrateUsageClientIndex(ctx context.Context, db *bolt.DB, batchSize int, afterBatch func()) error {
	var observed func(time.Duration)
	if afterBatch != nil {
		observed = func(time.Duration) { afterBatch() }
	}
	return migrateUsageClientIndexObserved(ctx, db, batchSize, observed)
}

// The optional observer runs after committed build batches, never in a bolt
// transaction. It is call-local instrumentation for recovery tests/benchmarks.
func migrateUsageClientIndexObserved(ctx context.Context, db *bolt.DB, batchSize int, afterBatch func(time.Duration)) error {
	size := clientIndexBatchSize(batchSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exhausted, done := false, false
		var started time.Time
		if afterBatch != nil {
			started = time.Now()
		}
		err := db.Update(func(tx *bolt.Tx) error {
			for _, name := range []string{BucketMeta, BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours} {
				if tx.Bucket([]byte(name)) == nil {
					return ErrUsageClientIndexIncomplete
				}
			}
			meta := tx.Bucket([]byte(BucketMeta))
			if hasUsageQuarantine(tx) {
				return fmt.Errorf("%w: unresolved quarantined raw events", ErrUsageClientIndexIncomplete)
			}
			if meta.Get([]byte(usageClientIndexDone)) != nil {
				done = true
				return nil
			}
			cursor := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			last := meta.Get([]byte(usageClientIndexCursor))
			k, v := cursor.First()
			if last != nil {
				k, v = cursor.Seek(last)
				if bytes.Equal(k, last) {
					k, v = cursor.Next()
				}
			}
			var next []byte
			for n := 0; k != nil && n < size; n++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				var event usage.Event
				if err := json.Unmarshal(v, &event); err != nil {
					return fmt.Errorf("%w: cannot decode raw event during migration", ErrUsageClientIndexIncomplete)
				}
				if err := addClientEventIndex(tx, event, k, v); err != nil {
					return err
				}
				next = append(next[:0], k...)
				k, v = cursor.Next()
			}
			exhausted = k == nil
			if next != nil {
				return meta.Put([]byte(usageClientIndexCursor), next)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("build usage client index: %w", err)
		}
		if done {
			return nil
		}
		if afterBatch != nil {
			afterBatch(time.Since(started))
		}
		if exhausted {
			break
		}
	}
	return verifyUsageClientIndex(ctx, db, size)
}

// scanIndexVerification bounds the number of keys visited, not just decoded
// records. Each callback is inside a short View, and cannot retain bolt slices.
func scanIndexVerification(ctx context.Context, db *bolt.DB, bucket string, size int, fn func(*bolt.Tx, []byte, []byte) error) error {
	var last []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exhausted := false
		err := db.View(func(tx *bolt.Tx) error {
			cursor := tx.Bucket([]byte(bucket)).Cursor()
			k, v := cursor.First()
			if last != nil {
				k, v = cursor.Seek(last)
				if bytes.Equal(k, last) {
					k, v = cursor.Next()
				}
			}
			for n := 0; k != nil && n < size; n++ {
				if err := fn(tx, k, v); err != nil {
					return err
				}
				last = append(last[:0], k...)
				k, v = cursor.Next()
			}
			exhausted = k == nil
			return nil
		})
		if err != nil {
			return err
		}
		if exhausted {
			return nil
		}
	}
}

func verifyUsageClientIndex(ctx context.Context, db *bolt.DB, size int) error {
	var verifiedRecords uint64
	err := scanIndexVerification(ctx, db, BucketUsageEvents, size, func(tx *bolt.Tx, k, v []byte) error {
		var event usage.Event
		if json.Unmarshal(v, &event) != nil || !bytes.Equal(tx.Bucket([]byte(BucketUsageEventsByClient)).Get(clientEventIndexKey(event, k)), k) {
			return fmt.Errorf("%w: raw event lacks matching client index", ErrUsageClientIndexIncomplete)
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = scanIndexVerification(ctx, db, BucketUsageEventsByClient, size, func(tx *bolt.Tx, k, v []byte) error {
		data := tx.Bucket([]byte(BucketUsageEvents)).Get(v)
		var event usage.Event
		if data == nil || json.Unmarshal(data, &event) != nil || !bytes.Equal(k, clientEventIndexKey(event, v)) {
			return fmt.Errorf("%w: orphaned or redirected client index", ErrUsageClientIndexIncomplete)
		}
		state, err := loadClientHour(tx, clientHourKey(event.ClientName, event.Timestamp))
		if err != nil || state.Count == 0 {
			return ErrUsageClientIndexIncomplete
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Listing keys is bounded; each actual segment verification gets its own
	// read transaction. New/backdated writes maintain already checked segments
	// atomically and need no global-revision restart.
	var last []byte
	for {
		var keys [][]byte
		err = db.View(func(tx *bolt.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			c := tx.Bucket([]byte(BucketUsageClientHours)).Cursor()
			k, _ := c.First()
			if last != nil {
				k, _ = c.Seek(last)
				if bytes.Equal(k, last) {
					k, _ = c.Next()
				}
			}
			for n := 0; k != nil && n < size; n++ {
				keys = append(keys, bytes.Clone(k))
				k, _ = c.Next()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			break
		}
		for _, key := range keys {
			var count uint64
			for attempt := 0; attempt < clientIndexRetries; attempt++ {
				count, err = verifyClientHourSegment(ctx, db, key)
				if !errors.Is(err, ErrUsageClientIndexChanged) {
					break
				}
			}
			if err != nil {
				return err
			}
			verifiedRecords += count
		}
		last = keys[len(keys)-1]
	}
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hasUsageQuarantine(tx) {
			return ErrUsageClientIndexIncomplete
		}
		marker, err := json.Marshal(struct {
			CompletedAt time.Time `json:"completed_at"`
			Records     uint64    `json:"verified_records"`
			Revision    uint64    `json:"raw_revision"`
		}{time.Now().UTC(), verifiedRecords, rawRevision(tx)})
		if err != nil {
			return err
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Delete([]byte(usageClientIndexCursor)); err != nil {
			return err
		}
		return meta.Put([]byte(usageClientIndexDone), marker)
	})
}

func verifyClientHourSegment(ctx context.Context, db *bolt.DB, key []byte) (uint64, error) {
	var count uint64
	err := db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts := strings.SplitN(string(key), "|", 2)
		if len(parts) != 2 {
			return ErrUsageClientIndexIncomplete
		}
		hour, err := time.Parse(hourLayout, parts[1])
		if err != nil {
			return ErrUsageClientIndexIncomplete
		}
		var state usageClientHour
		if json.Unmarshal(tx.Bucket([]byte(BucketUsageClientHours)).Get(key), &state) != nil {
			return ErrUsageClientIndexIncomplete
		}
		start := parts[0] + "|" + hour.Format(clientIndexTimeLayout)
		end := parts[0] + "|" + hour.Add(time.Hour).Format(clientIndexTimeLayout)
		var digest [32]byte
		c := tx.Bucket([]byte(BucketUsageEventsByClient)).Cursor()
		for k, v := c.Seek([]byte(start)); k != nil && string(k) < end; k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			data := tx.Bucket([]byte(BucketUsageEvents)).Get(v)
			var event usage.Event
			if data == nil || json.Unmarshal(data, &event) != nil || !bytes.Equal(k, clientEventIndexKey(event, v)) {
				return ErrUsageClientIndexIncomplete
			}
			count++
			xorDigest(&digest, rawDigest(v, data))
		}
		// State, revision, count, digest and raw bytes share this snapshot.
		current, err := loadClientHour(tx, key)
		if err != nil {
			return err
		}
		if current.Revision != state.Revision {
			return ErrUsageClientIndexChanged
		}
		if state.Count != count || state.Digest != digest {
			return fmt.Errorf("%w: client/hour count or checksum mismatch", ErrUsageClientIndexIncomplete)
		}
		return nil
	})
	return count, err
}
