package nosql

import (
	"bytes"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// usageClientIndexNonCanonicalKey marks a database whose client index holds at
// least one entry that normal writes cannot produce. While it is set, identity
// lookups scan the whole index instead of probing exact keys.
const usageClientIndexNonCanonicalKey = "usage_client_index_noncanonical_v1"

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

func clientIndexAllCanonical(index *bolt.Bucket) bool {
	canonical := true
	_ = index.ForEach(func(k, v []byte) error {
		if !clientIndexEntryCanonical(k, v) {
			canonical = false
		}
		return nil
	})
	return canonical
}

// markClientIndexNonCanonical records the anomaly durably; repeating it is a
// no-op.
func markClientIndexNonCanonical(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil || meta.Get([]byte(usageClientIndexNonCanonicalKey)) != nil {
			return nil
		}
		return meta.Put([]byte(usageClientIndexNonCanonicalKey), []byte("1"))
	})
}

func clientIndexNonCanonicalMarked(tx *bolt.Tx) bool {
	meta := tx.Bucket([]byte(BucketMeta))
	return meta != nil && meta.Get([]byte(usageClientIndexNonCanonicalKey)) != nil
}
