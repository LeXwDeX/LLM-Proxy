package nosql

import (
	"encoding/json"
	"fmt"
	"hash/maphash"
	"log/slog"
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
}{
	decodeFailed: make(map[proofRawDigest]struct{}),
	scopes:       make(map[proofRawDigest]proofRawScope),
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
