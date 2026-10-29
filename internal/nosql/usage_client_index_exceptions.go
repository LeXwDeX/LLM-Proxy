package nosql

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// bucketUsageClientIndexExceptions is the set of client index entries whose
// shape normal writes do not produce (clientIndexEntryCanonical is false). The
// keyed identity probe only sees canonical entries, so lookupUsageIdentity
// consults this set for the rest. Legitimate sources are ReindexUsageEvent
// (it rewrites an event's timestamp under an unchanged raw key) and legacy
// rows whose key stamp and data time disagree; out-of-band damage is picked up
// by the next WarmUsageProofScanCache.
//
// Set keys are hex(rawKey)|hex(indexKey), so all entries of one raw key share
// a prefix that hex makes unambiguous; the value is the index key. The set is
// maintained in the transaction that writes or deletes the index entry
// (addClientEventIndex and removeClientEventIndex are the only such writers;
// TestClientIndexWritersAreEnumerated guards that), so it never lags behind a
// supported writer.
const bucketUsageClientIndexExceptions = "usage_client_index_exceptions_v1"

// usageClientIndexNonCanonicalKey is the meta marker written by an earlier
// revision: it forced full scans forever. It is removed at warm-up.
const usageClientIndexNonCanonicalKey = "usage_client_index_noncanonical_v1"

// clientIndexExceptionLimit bounds the set a lookup relies on. Above it the
// lookup scans the whole index instead (and warns once).
const clientIndexExceptionLimit = 10000

var (
	exceptionCountKey   = []byte("#count") // no '|', never a set key prefix match
	exceptionOverflow   = ^uint64(0)
	exceptionScanWarned atomic.Bool
	// clientIndexFullScans counts identity lookups that fell back to scanning
	// the whole index (test and diagnostics hook).
	clientIndexFullScans atomic.Int64
)

// clientIndexEntryCanonical reports whether the entry has the shape
// clientEventIndexKey writes: hex(client)|stamp(value)|value, where stamp is
// the value's own timestamp rendered with clientIndexTimeLayout.
func clientIndexEntryCanonical(key, value []byte) bool {
	sep := bytes.IndexByte(key, '|')
	if sep < 0 {
		return false
	}
	stamp, _, found := strings.Cut(string(value), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil || at.UTC().Format(time.RFC3339Nano) != stamp {
		return false
	}
	return string(key[sep+1:]) == at.UTC().Format(clientIndexTimeLayout)+"|"+string(value)
}

func exceptionPrefix(rawKey []byte) []byte {
	return append([]byte(hex.EncodeToString(rawKey)), '|')
}

func exceptionKey(indexKey, rawKey []byte) []byte {
	return append(exceptionPrefix(rawKey), hex.EncodeToString(indexKey)...)
}

func exceptionCount(b *bolt.Bucket) uint64 {
	if b == nil {
		return 0
	}
	if v := b.Get(exceptionCountKey); len(v) == 8 {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}

func putExceptionCount(b *bolt.Bucket, n uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	return b.Put(exceptionCountKey, buf[:])
}

// registerClientIndexException records indexKey if it is not canonical. It
// must run in the transaction that writes the index entry. Repeating it is a
// no-op.
func registerClientIndexException(tx *bolt.Tx, indexKey, rawKey []byte) error {
	if clientIndexEntryCanonical(indexKey, rawKey) {
		return nil
	}
	b, err := tx.CreateBucketIfNotExists([]byte(bucketUsageClientIndexExceptions))
	if err != nil {
		return err
	}
	ek := exceptionKey(indexKey, rawKey)
	if b.Get(ek) != nil {
		return nil
	}
	if err := b.Put(ek, indexKey); err != nil {
		return err
	}
	if n := exceptionCount(b); n != exceptionOverflow {
		return putExceptionCount(b, n+1)
	}
	return nil
}

// unregisterClientIndexException drops the entry in the transaction that
// deletes the index entry. Absent entries are ignored.
func unregisterClientIndexException(tx *bolt.Tx, indexKey, rawKey []byte) error {
	b := tx.Bucket([]byte(bucketUsageClientIndexExceptions))
	if b == nil {
		return nil
	}
	ek := exceptionKey(indexKey, rawKey)
	if b.Get(ek) == nil {
		return nil
	}
	if err := b.Delete(ek); err != nil {
		return err
	}
	if n := exceptionCount(b); n != exceptionOverflow && n > 0 {
		return putExceptionCount(b, n-1)
	}
	return nil
}

// clientIndexExceptionReferences reports whether a registered non-canonical
// index entry points at rawKey. overflow means the set is too large to trust
// and the caller must scan the index.
func clientIndexExceptionReferences(tx *bolt.Tx, rawKey []byte) (hit, overflow bool) {
	b := tx.Bucket([]byte(bucketUsageClientIndexExceptions))
	if b == nil {
		return false, false
	}
	if n := exceptionCount(b); n > clientIndexExceptionLimit {
		return false, true
	}
	prefix := exceptionPrefix(rawKey)
	k, _ := b.Cursor().Seek(prefix)
	return k != nil && bytes.HasPrefix(k, prefix), false
}

func warnClientIndexScan() {
	if exceptionScanWarned.CompareAndSwap(false, true) {
		slog.Warn("usage client index has too many non-canonical entries; identity lookups scan the whole index", "limit", clientIndexExceptionLimit)
	}
}

// collectClientIndexExceptions lists the non-canonical entries, stopping after
// limit+1 of them (more than limit means overflow).
func collectClientIndexExceptions(index *bolt.Bucket) (keys, values [][]byte) {
	_ = index.ForEach(func(k, v []byte) error {
		if len(keys) > clientIndexExceptionLimit {
			return nil
		}
		if !clientIndexEntryCanonical(k, v) {
			keys = append(keys, append([]byte(nil), k...))
			values = append(values, append([]byte(nil), v...))
		}
		return nil
	})
	return keys, values
}

// rebuildClientIndexExceptions makes the set equal to the index's current
// non-canonical entries and drops the legacy marker. It only writes when the
// stored set differs, so a clean database is not touched. Safe to repeat.
func rebuildClientIndexExceptions(db *bolt.DB) error {
	needed := false
	if err := db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil && meta.Get([]byte(usageClientIndexNonCanonicalKey)) != nil {
			needed = true
		}
		var keys, values [][]byte
		if index := tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil {
			keys, values = collectClientIndexExceptions(index)
		}
		set := tx.Bucket([]byte(bucketUsageClientIndexExceptions))
		stored := exceptionCount(set)
		if len(keys) > clientIndexExceptionLimit {
			needed = needed || stored != exceptionOverflow
			return nil
		}
		members := 0
		if set != nil {
			_ = set.ForEach(func(k, _ []byte) error {
				if !bytes.Equal(k, exceptionCountKey) {
					members++
				}
				return nil
			})
		}
		if stored != uint64(len(keys)) || members != len(keys) {
			needed = true
			return nil
		}
		for i := range keys {
			if set.Get(exceptionKey(keys[i], values[i])) == nil {
				needed = true
				return nil
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	return db.Update(func(tx *bolt.Tx) error {
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
			if err := meta.Delete([]byte(usageClientIndexNonCanonicalKey)); err != nil {
				return err
			}
		}
		if tx.Bucket([]byte(bucketUsageClientIndexExceptions)) != nil {
			if err := tx.DeleteBucket([]byte(bucketUsageClientIndexExceptions)); err != nil {
				return err
			}
		}
		var keys, values [][]byte
		if index := tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil {
			keys, values = collectClientIndexExceptions(index)
		}
		if len(keys) == 0 {
			return nil
		}
		b, err := tx.CreateBucket([]byte(bucketUsageClientIndexExceptions))
		if err != nil {
			return err
		}
		if len(keys) > clientIndexExceptionLimit {
			return putExceptionCount(b, exceptionOverflow)
		}
		for i := range keys {
			if err := b.Put(exceptionKey(keys[i], values[i]), keys[i]); err != nil {
				return err
			}
		}
		return putExceptionCount(b, uint64(len(keys)))
	})
}
