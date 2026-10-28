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
	var lost bool
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return ErrUsageClientIndexIncomplete
		}
		lost = meta.Get([]byte(usageClientIndexSourceMissing)) != nil
		return nil
	}); err != nil {
		return err
	}
	if lost {
		return rebuildLostV1ClientIndex(ctx, db, size, afterBatch)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exhausted, done, verifyLegacy := false, false, false
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
			if meta.Get([]byte(usageClientIndexSourceMissing)) != nil || meta.Bucket([]byte(usageClientIndexSourceMissing)) != nil {
				return fmt.Errorf("%w: client index source was lost", ErrUsageClientIndexIncomplete)
			}
			progress, err := loadIndexProgress(tx)
			if err != nil {
				return err
			}
			if meta.Get([]byte(usageClientIndexDone)) != nil {
				done = true
				verifyLegacy = progress == nil
				return nil
			}
			if progress == nil {
				progress, err = startIndexProgress(ctx, tx)
				if err != nil {
					return err
				}
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
			refs := &indexRawReferences{tx: tx}
			for n := 0; k != nil && progress.Upper != nil && bytes.Compare(k, progress.Upper) <= 0 && n < size; n++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				var event usage.Event
				if err := json.Unmarshal(v, &event); err != nil {
					return fmt.Errorf("%w: cannot decode raw event during migration", ErrUsageClientIndexIncomplete)
				}
				if tx.Bucket([]byte(indexProofsV2)).Get(k) == nil {
					_, _, invalid, _, err := indexProofForRaw(ctx, tx, refs, progress, k, v, event)
					if err != nil {
						return err
					}
					if invalid {
						return indexV2Error("missing proof for indexed raw event")
					}
				}
				if err := addClientEventIndex(tx, event, k, v); err != nil {
					return err
				}
				if refs.keys != nil {
					refs.keys[string(k)] = true
				}
				if err := writeIndexProof(tx, progress, k, v, event, "scan"); err != nil {
					return err
				}
				next = append(next[:0], k...)
				k, v = cursor.Next()
			}
			exhausted = k == nil || progress.Upper == nil || bytes.Compare(k, progress.Upper) > 0
			if next != nil {
				if err := meta.Put([]byte(usageClientIndexCursor), next); err != nil {
					return err
				}
				progress.Through = next
				return saveIndexProgress(tx, progress)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("build usage client index: %w", err)
		}
		if done {
			if verifyLegacy {
				return verifyUsageClientIndex(ctx, db, size)
			}
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

// Rebuilding the old derived index is not v2 certification. Keep the source
// loss marker and every v2 proof unchanged, so accounting cannot trust it.
func rebuildLostV1ClientIndex(ctx context.Context, db *bolt.DB, size int, afterBatch func(time.Duration)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		exhausted, done := false, false
		err := db.Update(func(tx *bolt.Tx) error {
			meta := tx.Bucket([]byte(BucketMeta))
			if meta == nil || string(meta.Get([]byte(usageClientIndexSourceMissing))) != "1" || hasUsageQuarantine(tx) {
				return ErrUsageClientIndexIncomplete
			}
			p, err := loadIndexProgressState(tx, false)
			if err != nil || (p != nil && p.Phase != "complete") {
				return ErrUsageClientIndexIncomplete
			}
			if meta.Get([]byte(usageClientIndexDone)) != nil {
				done = true
				return nil
			}
			for _, name := range []string{BucketUsageEvents, BucketUsageEventsByClient, BucketUsageClientHours} {
				if tx.Bucket([]byte(name)) == nil {
					return ErrUsageClientIndexIncomplete
				}
			}
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			last := meta.Get([]byte(usageClientIndexCursor))
			k, v := c.First()
			if last != nil {
				k, v = c.Seek(last)
				if bytes.Equal(k, last) {
					k, v = c.Next()
				}
			}
			var next []byte
			for n := 0; k != nil && n < size; n++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				var event usage.Event
				if json.Unmarshal(v, &event) != nil {
					return ErrUsageClientIndexIncomplete
				}
				if err := addClientEventIndex(tx, event, k, v); err != nil {
					return err
				}
				next = bytes.Clone(k)
				k, v = c.Next()
			}
			exhausted = k == nil
			if next != nil {
				return meta.Put([]byte(usageClientIndexCursor), next)
			}
			return nil
		})
		if err != nil {
			return err
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
	return verifyUsageClientIndexMode(ctx, db, size, true)
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
			b := tx.Bucket([]byte(bucket))
			if b == nil {
				return ErrUsageClientIndexIncomplete
			}
			cursor := b.Cursor()
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
	return verifyUsageClientIndexMode(ctx, db, size, false)
}

func verifyUsageClientIndexMode(ctx context.Context, db *bolt.DB, size int, untrustedV1 bool) error {
	var verifiedRecords uint64
	err := scanIndexVerification(ctx, db, BucketUsageEvents, size, func(tx *bolt.Tx, k, v []byte) error {
		var event usage.Event
		if json.Unmarshal(v, &event) != nil || !bytes.Equal(tx.Bucket([]byte(BucketUsageEventsByClient)).Get(clientEventIndexKey(event, k)), k) {
			return fmt.Errorf("%w: raw event lacks matching client index", ErrUsageClientIndexIncomplete)
		}
		if !untrustedV1 {
			p, err := loadIndexProgress(tx)
			if err != nil {
				return err
			}
			if p != nil {
				proofs := tx.Bucket([]byte(indexProofsV2))
				if proofs.Get(k) == nil {
					_, owned, err := readRawOwner(tx, k)
					if err != nil {
						return err
					}
					if !owned {
						return indexV2Error("missing raw index proof")
					}
				} else {
					proof, err := readIndexProof(proofs, p, k)
					digest := rawDigest(k, v)
					if err != nil || proof.RawDigest != fmt.Sprintf("%x", digest) || !bytes.Equal(proof.IndexKey, clientEventIndexKey(event, k)) || (proof.Origin == "scan" && (len(p.Through) == 0 || bytes.Compare(k, p.Through) > 0)) {
						return indexV2Error("invalid raw index proof")
					}
				}
			}
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
	var hasV2 bool
	err = db.View(func(tx *bolt.Tx) error {
		if untrustedV1 {
			return nil
		}
		p, err := loadIndexProgress(tx)
		hasV2 = p != nil
		return err
	})
	if err != nil {
		return err
	}
	if hasV2 {
		err = scanIndexVerification(ctx, db, indexProofsV2, size, func(tx *bolt.Tx, k, _ []byte) error {
			p, err := loadIndexProgress(tx)
			if err != nil || p == nil {
				return ErrUsageClientIndexIncomplete
			}
			proof, err := readIndexProof(tx.Bucket([]byte(indexProofsV2)), p, k)
			if err != nil {
				return err
			}
			if raw := tx.Bucket([]byte(BucketUsageEvents)).Get(k); raw != nil {
				var event usage.Event
				digest := rawDigest(k, raw)
				if json.Unmarshal(raw, &event) != nil || proof.RawDigest != fmt.Sprintf("%x", digest) || !bytes.Equal(proof.IndexKey, clientEventIndexKey(event, k)) {
					return indexV2Error("proof disagrees with raw event")
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
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
		if untrustedV1 {
			if string(meta.Get([]byte(usageClientIndexSourceMissing))) != "1" {
				return ErrUsageClientIndexIncomplete
			}
			if _, err := loadIndexProgressState(tx, false); err != nil {
				return err
			}
			if err := meta.Delete([]byte(usageClientIndexCursor)); err != nil {
				return err
			}
			return meta.Put([]byte(usageClientIndexDone), marker)
		}
		progress, err := loadIndexProgress(tx)
		if err != nil {
			return ErrUsageClientIndexIncomplete
		}
		if meta.Get([]byte(usageClientIndexSourceMissing)) != nil || meta.Bucket([]byte(usageClientIndexSourceMissing)) != nil {
			return ErrUsageClientIndexIncomplete
		}
		if progress == nil {
			if meta.Get([]byte(usageClientIndexDone)) == nil || meta.Get([]byte(usageClientIndexCursor)) != nil {
				return ErrUsageClientIndexIncomplete
			}
			return nil
		}
		if progress.Phase == "complete" {
			return meta.Put([]byte(usageClientIndexDone), marker)
		}
		if err := meta.Delete([]byte(usageClientIndexCursor)); err != nil {
			return err
		}
		if err := meta.Put([]byte(usageClientIndexDone), marker); err != nil {
			return err
		}
		progress.Phase = "complete"
		return saveIndexProgress(tx, progress)
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
