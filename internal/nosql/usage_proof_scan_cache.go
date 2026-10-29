package nosql

import (
	"encoding/json"
	"fmt"
	"hash/maphash"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unique"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// proofRawParses counts full parses of raw usage rows by the proof helpers.
var proofRawParses atomic.Int64

// Proofs on upgraded databases scan every raw row inside the bbolt write
// transaction. Legacy rows fail decodeProofEvent and are classified by
// backfillRawScope; both are pure functions of the row bytes, so results are
// memoized by content digest. A rewritten row has a new digest and is parsed
// again, so no invalidation is needed and every proof decision is unchanged.
// The digest is two independently seeded 64-bit maphashes: per-process random
// seeds cannot be targeted, and 128 bits make accidental collisions negligible.
const proofScanCacheMaxEntries = 1 << 19

// proofScanCacheFull warns once that rows beyond the cap are parsed on every
// proof scan again, which brings back slow usage writes on very large histories.
var proofScanCacheFull sync.Once

func warnProofScanCacheFull() {
	proofScanCacheFull.Do(func() {
		slog.Warn("usage proof scan cache is full; further legacy rows are parsed on every usage write", "max_entries", proofScanCacheMaxEntries)
	})
}

type proofRawScope struct {
	clientName string
	clientHash string // receiptClientHash(clientName) when clientName != ""
	model      string
	timestamp  time.Time
	ambiguous  bool
	valid      bool
}

type proofRawDigest [2]uint64

var proofDigestSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

func proofDigest(data []byte) proofRawDigest {
	return proofRawDigest{maphash.Bytes(proofDigestSeeds[0], data), maphash.Bytes(proofDigestSeeds[1], data)}
}

var proofScanCache = struct {
	sync.RWMutex
	decodeFailed map[proofRawDigest]struct{}
	scopes       map[proofRawDigest]proofRawScope
	canonical    map[proofRawDigest]struct{}
}{
	decodeFailed: make(map[proofRawDigest]struct{}),
	scopes:       make(map[proofRawDigest]proofRawScope),
	canonical:    make(map[proofRawDigest]struct{}),
}

// decodeProofEventCached is decodeProofEvent for full raw-bucket scans. Only
// failures are memoized; callers must not read event when it returns false.
func decodeProofEventCached(data []byte, event *usage.Event) bool {
	sum := proofDigest(data)
	proofScanCache.RLock()
	_, failed := proofScanCache.decodeFailed[sum]
	proofScanCache.RUnlock()
	if failed {
		return false
	}
	if decodeProofEvent(data, event) {
		return true
	}
	proofScanCache.Lock()
	if len(proofScanCache.decodeFailed) < proofScanCacheMaxEntries {
		proofScanCache.decodeFailed[sum] = struct{}{}
	} else {
		defer warnProofScanCacheFull()
	}
	proofScanCache.Unlock()
	return false
}

func proofRowDigest(key, data []byte) proofRawDigest {
	var sum proofRawDigest
	for i := range sum {
		var h maphash.Hash
		h.SetSeed(proofDigestSeeds[i])
		h.Write(key)
		h.WriteByte(0)
		h.Write(data)
		sum[i] = h.Sum64()
	}
	return sum
}

// canonicalRawRowCached reports decodeProofEvent(data) && usageKey(event)==key.
// Only positive results are memoized, keyed by the digest of key and bytes, so
// a rewritten row or a different key is classified again.
func canonicalRawRowCached(key, data []byte) bool {
	sum := proofRowDigest(key, data)
	proofScanCache.RLock()
	_, ok := proofScanCache.canonical[sum]
	proofScanCache.RUnlock()
	if ok {
		return true
	}
	var event usage.Event
	// The failure memo keeps legacy rows from being parsed again on every scan.
	if !decodeProofEventCached(data, &event) || usageKey(event.Timestamp, event.RecordID) != string(key) {
		return false
	}
	proofScanCache.Lock()
	if len(proofScanCache.canonical) < proofScanCacheMaxEntries {
		proofScanCache.canonical[sum] = struct{}{}
	} else {
		defer warnProofScanCacheFull()
	}
	proofScanCache.Unlock()
	return true
}

// proofScopeSkipEnabled lets differential tests run the exhaustive pre-#120
// scans as the reference for the scoped skip below.
var proofScopeSkipEnabled = true

// keyHourOutside reports whether the key carries a parseable timestamp stamp
// whose hour is not in hours.
func keyHourOutside(key []byte, hours map[time.Time]bool) bool {
	if !proofScopeSkipEnabled {
		return false
	}
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return found && err == nil && !hours[at.UTC().Truncate(time.Hour)]
}

// canonicalRowOutsideHours is true for a canonical raw row whose own hour is
// not in hours. Such a row can neither match a cell of those hours nor an
// index/aggregate expectation for them, so a scan may skip it. Canonical rows
// have key == usageKey(event.Timestamp, ...), hence the key stamp is the
// event's own instant.
func canonicalRowOutsideHours(key, data []byte, hours map[time.Time]bool) bool {
	return keyHourOutside(key, hours) && canonicalRawRowCached(key, data)
}

// backfillRawScopeCached returns the fields of backfillRawScope used by the
// overlap checks, plus json.Valid of the same bytes.
func backfillRawScopeCached(data []byte) proofRawScope {
	sum := proofDigest(data)
	proofScanCache.RLock()
	scope, ok := proofScanCache.scopes[sum]
	proofScanCache.RUnlock()
	if ok {
		return scope
	}
	event, ambiguous := backfillRawScope(data)
	// Few distinct clients and models recur across many rows; intern them.
	scope = proofRawScope{
		clientName: unique.Make(event.ClientName).Value(),
		clientHash: unique.Make(receiptClientHashIfNamed(event.ClientName)).Value(),
		model:      unique.Make(event.Model).Value(),
		timestamp:  event.Timestamp,
		ambiguous:  ambiguous,
		valid:      json.Valid(data),
	}
	proofScanCache.Lock()
	if len(proofScanCache.scopes) < proofScanCacheMaxEntries {
		proofScanCache.scopes[sum] = scope
	} else {
		defer warnProofScanCacheFull()
	}
	proofScanCache.Unlock()
	return scope
}

func receiptClientHashIfNamed(name string) string {
	if name == "" {
		return ""
	}
	return receiptClientHash(name)
}

// WarmUsageProofScanCache classifies raw rows in a read transaction so the
// first usage write after startup does not parse the whole history while it
// holds the bbolt writer. Databases tracked since creation never run the
// legacy scans and are skipped. It returns the number of rows visited.
func WarmUsageProofScanCache(db *bolt.DB) (int, error) {
	visited := 0
	err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if meta == nil || raw == nil {
			return fmt.Errorf("usage proof cache warm-up: buckets missing")
		}
		if string(meta.Get([]byte(usageAccountingProofTrackedKey))) == "1" {
			return nil
		}
		return raw.ForEach(func(k, v []byte) error {
			visited++
			var event usage.Event
			if !decodeProofEventCached(v, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
				backfillRawScopeCached(v)
			}
			return nil
		})
	})
	return visited, err
}

// warmCanonicalRawRows classifies raw rows in a read transaction, so the
// retention writer finds them already memoized and only the rows of the
// relevant hours are decoded under the bbolt write lock.
func warmCanonicalRawRows(db *bolt.DB) error {
	return db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if raw == nil {
			return nil
		}
		return raw.ForEach(func(k, v []byte) error {
			canonicalRawRowCached(k, v)
			return nil
		})
	})
}
