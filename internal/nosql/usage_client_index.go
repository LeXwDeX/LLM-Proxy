package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const (
	BucketUsageEventsByClient     = "usage_events_by_client_v1"
	BucketUsageClientHours        = "usage_client_hours_v1"
	usageClientIndexDone          = "usage_client_index_complete_v1"
	usageClientIndexCursor        = "usage_client_index_cursor_v1"
	usageClientIndexSourceMissing = "usage_client_index_source_missing_v1"
	usageRawRevisionKey           = "usage_raw_revision_v1"
	clientIndexTimeLayout         = "2006-01-02T15:04:05.000000000Z"
	clientIndexRetries            = 3
)

var (
	// Incomplete means callers must use the legacy path or fail closed. Never
	// use a partial slice to replace quota counters or report a complete bill.
	ErrUsageClientIndexIncomplete = errors.New("usage client index is incomplete or inconsistent")
	ErrUsageClientIndexChanged    = errors.New("usage client index did not stabilize")
)

// Each client/hour tracks retained raw records, independently of the permanent
// rollups. XOR of SHA-256(key NUL value), together with Count, detects missing,
// duplicate, redirected and modified records. Revision changes on every index
// insertion/deletion, in the same transaction as the raw record mutation.
type usageClientHour struct {
	Revision uint64
	Count    uint64
	Digest   [sha256.Size]byte
}

func clientIndexPrefix(client string) string {
	// Keep client matching case-sensitive, just as Record and quotaAggKey do.
	return hex.EncodeToString([]byte(strings.TrimSpace(client))) + "|"
}

func clientEventIndexKey(event usage.Event, rawKey []byte) []byte {
	return []byte(clientIndexPrefix(event.ClientName) + event.Timestamp.UTC().Format(clientIndexTimeLayout) + "|" + string(rawKey))
}

func clientHourKey(client string, hour time.Time) []byte {
	return []byte(clientIndexPrefix(client) + hour.UTC().Truncate(time.Hour).Format(hourLayout))
}

func rawDigest(key, value []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(key)
	h.Write([]byte{0})
	h.Write(value)
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func xorDigest(dst *[sha256.Size]byte, sum [sha256.Size]byte) {
	for i := range dst {
		dst[i] ^= sum[i]
	}
}

func loadClientHour(tx *bolt.Tx, key []byte) (usageClientHour, error) {
	var state usageClientHour
	b := tx.Bucket([]byte(BucketUsageClientHours))
	if b == nil {
		return state, ErrUsageClientIndexIncomplete
	}
	if data := b.Get(key); data != nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return state, fmt.Errorf("%w: invalid client/hour state", ErrUsageClientIndexIncomplete)
		}
	}
	return state, nil
}

func updateClientHour(tx *bolt.Tx, event usage.Event, key, data []byte, remove bool) error {
	hourKey := clientHourKey(event.ClientName, event.Timestamp)
	state, err := loadClientHour(tx, hourKey)
	if err != nil {
		return err
	}
	if remove {
		if state.Count == 0 {
			return ErrUsageClientIndexIncomplete
		}
		state.Count--
	} else {
		state.Count++
	}
	state.Revision++
	xorDigest(&state.Digest, rawDigest(key, data))
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(BucketUsageClientHours)).Put(hourKey, encoded)
}

// addClientEventIndex is idempotent, including during migration or journal
// replay. It must run inside the raw record's write transaction.
func addClientEventIndex(tx *bolt.Tx, event usage.Event, rawKey, data []byte) error {
	b := tx.Bucket([]byte(BucketUsageEventsByClient))
	if b == nil {
		return ErrUsageClientIndexIncomplete
	}
	key := clientEventIndexKey(event, rawKey)
	if old := b.Get(key); old != nil {
		if !bytes.Equal(old, rawKey) {
			return ErrUsageClientIndexIncomplete
		}
		return nil
	}
	if err := b.Put(key, rawKey); err != nil {
		return err
	}
	if err := registerClientIndexException(tx, key, rawKey); err != nil {
		return err
	}
	return updateClientHour(tx, event, rawKey, data, false)
}

func removeClientEventIndex(tx *bolt.Tx, rawKey, data []byte) error {
	var event usage.Event
	if err := json.Unmarshal(data, &event); err != nil {
		// Without an owner we cannot remove its derived key safely. Preserve
		// both records, rather than committing a raw deletion with an orphan.
		return fmt.Errorf("%w: cannot identify client of corrupt raw event", ErrUsageClientIndexIncomplete)
	}
	b := tx.Bucket([]byte(BucketUsageEventsByClient))
	if b == nil {
		return ErrUsageClientIndexIncomplete
	}
	key := clientEventIndexKey(event, rawKey)
	old := b.Get(key)
	if old == nil {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
			return ErrUsageClientIndexIncomplete
		}
		return nil // historical record not reached by migration yet
	}
	if !bytes.Equal(old, rawKey) {
		return ErrUsageClientIndexIncomplete
	}
	if err := b.Delete(key); err != nil {
		return err
	}
	if err := unregisterClientIndexException(tx, key, rawKey); err != nil {
		return err
	}
	return updateClientHour(tx, event, rawKey, data, true)
}

// ReindexUsageEvent maintains derived data when an offline maintenance tool
// rewrites a raw event without changing its key. The caller MUST update the raw
// value and affected rollups in this same write transaction. Keeping before/
// after bytes lets the tool preserve unknown JSON fields. Legacy databases with
// neither derived bucket need no update; a partially missing schema is an error.
// Success maintains the index, not accounting identity: existing evidence is
// retained, so changed raw bytes can make exact accounting unavailable.
func ReindexUsageEvent(tx *bolt.Tx, rawKey, before, after []byte) error {
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	hours := tx.Bucket([]byte(BucketUsageClientHours))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if index == nil && hours == nil {
		return nil
	}
	if index == nil || hours == nil || raw == nil || tx.Bucket([]byte(BucketMeta)) == nil {
		return ErrUsageClientIndexIncomplete
	}
	var event usage.Event
	if json.Unmarshal(after, &event) != nil || !bytes.Equal(raw.Get(rawKey), before) {
		return ErrUsageClientIndexIncomplete
	}
	progress, err := loadIndexProgress(tx)
	if err != nil {
		return err
	}
	if progress != nil {
		_, owned, err := readRawOwner(tx, rawKey)
		if err != nil {
			return err
		}
		if !owned {
			proofs := tx.Bucket([]byte(indexProofsV2))
			if proofs.Get(rawKey) != nil {
				var original usage.Event
				if json.Unmarshal(before, &original) != nil {
					return ErrUsageClientIndexIncomplete
				}
				if err := writeIndexProof(tx, progress, rawKey, before, original, "atomic"); err != nil {
					return err
				}
			} else {
				if len(progress.Through) != 0 && bytes.Compare(rawKey, progress.Through) <= 0 {
					return indexV2Error("missing scanned proof")
				}
				var original usage.Event
				if json.Unmarshal(before, &original) != nil || index.Get(clientEventIndexKey(original, rawKey)) != nil {
					return indexV2Error("missing proof for indexed raw event")
				}
				refs := &indexRawReferences{tx: tx}
				indexed, err := refs.contains(context.Background(), rawKey)
				if err != nil {
					return err
				}
				if indexed {
					return indexV2Error("missing proof for indexed raw event")
				}
				if err := writeIndexProof(tx, progress, rawKey, after, event, "atomic"); err != nil {
					return err
				}
			}
		}
	}
	if err := removeClientEventIndex(tx, rawKey, before); err != nil {
		return err
	}
	if err := addClientEventIndex(tx, event, rawKey, after); err != nil {
		return err
	}
	// Issue #125: an offline rewrite may move the row to another hour.
	integrityMarkTx(tx)
	return bumpRawRevision(tx)
}

func rawRevision(tx *bolt.Tx) uint64 {
	data := tx.Bucket([]byte(BucketMeta)).Get([]byte(usageRawRevisionKey))
	if len(data) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data)
}

func bumpRawRevision(tx *bolt.Tx) error {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], rawRevision(tx)+1)
	return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageRawRevisionKey), data[:])
}

func clientIndexBatchSize(size int) int {
	if size <= 0 {
		return 256
	}
	if size > 4096 {
		return 4096
	}
	return size
}

func requireClientIndex(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil || meta.Get([]byte(usageClientIndexDone)) == nil || tx.Bucket([]byte(BucketUsageEventsByClient)) == nil || tx.Bucket([]byte(BucketUsageClientHours)) == nil || tx.Bucket([]byte(BucketUsageEvents)) == nil {
		return ErrUsageClientIndexIncomplete
	}
	if hasUsageQuarantine(tx) {
		return fmt.Errorf("%w: unresolved quarantined raw events", ErrUsageClientIndexIncomplete)
	}
	return nil
}

// readClientHourInTx reads a complete hour from a single accounting snapshot.
func readClientHourInTx(ctx context.Context, tx *bolt.Tx, client string, hour time.Time, decoded func(), verify func([]byte, []byte, usage.Event) error) ([]usage.Event, error) {
	state, err := loadClientHour(tx, clientHourKey(client, hour))
	if err != nil {
		return nil, err
	}
	start := []byte(clientIndexPrefix(client) + hour.Format(clientIndexTimeLayout))
	end := []byte(clientIndexPrefix(client) + hour.Add(time.Hour).Format(clientIndexTimeLayout))
	var count uint64
	var digest [sha256.Size]byte
	var events []usage.Event
	cursor := tx.Bucket([]byte(BucketUsageEventsByClient)).Cursor()
	for k, v := cursor.Seek(start); k != nil && bytes.Compare(k, end) < 0; k, v = cursor.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data := tx.Bucket([]byte(BucketUsageEvents)).Get(v)
		var event usage.Event
		if decoded != nil {
			decoded()
		}
		if data == nil || json.Unmarshal(data, &event) != nil || !bytes.Equal(k, clientEventIndexKey(event, v)) {
			return nil, ErrUsageClientIndexIncomplete
		}
		if verify != nil {
			if err := verify(v, data, event); err != nil {
				return nil, err
			}
		}
		count++
		xorDigest(&digest, rawDigest(v, data))
		events = append(events, event)
	}
	if count != state.Count || digest != state.Digest {
		return nil, ErrUsageClientIndexIncomplete
	}
	return events, nil
}

type costRawHourEvidence struct {
	client          string
	hour            time.Time
	count           uint64
	digest          [sha256.Size]byte
	relevant        bool
	owned           bool
	coveredCount    uint64
	coveredDigest   [sha256.Size]byte
	coverageInvalid bool
	proofInvalid    bool
}

// A completed index or an existing hour/index entry is evidence even if its
// completion marker is missing. A genuinely unindexed hour has none of these.
func validateCostRawHourEvidence(ctx context.Context, tx *bolt.Tx, progress *indexProgress, evidence map[string]*costRawHourEvidence) (bool, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	hours := tx.Bucket([]byte(BucketUsageClientHours))
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	complete := meta != nil && meta.Get([]byte(usageClientIndexDone)) != nil
	var cursor []byte
	if meta != nil {
		cursor = meta.Get([]byte(usageClientIndexCursor))
	}
	if cursor != nil {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		stamp, id, found := strings.Cut(string(cursor), "_")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if complete || !found || id == "" || err != nil || usageKey(at, id) != string(cursor) || raw == nil || (progress == nil && raw.Get(cursor) == nil) {
			return false, fmt.Errorf("%w: invalid client index migration cursor", ErrUsageHistoryIncomplete)
		}
	}
	allValid := true
	for key, observed := range evidence {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !observed.relevant {
			continue
		}
		if observed.proofInvalid {
			return false, fmt.Errorf("%w: invalid client index proof", ErrUsageHistoryIncomplete)
		}
		if cursor != nil && progress == nil && !observed.owned {
			return false, fmt.Errorf("%w: unproven v1 partial client index", ErrUsageHistoryIncomplete)
		}
		var stateData []byte
		if hours != nil {
			stateData = hours.Get([]byte(key))
		}
		indexed := false
		if index != nil {
			prefix := []byte(clientIndexPrefix(observed.client) + observed.hour.Format(clientIndexTimeLayout))
			end := []byte(clientIndexPrefix(observed.client) + observed.hour.Add(time.Hour).Format(clientIndexTimeLayout))
			k, _ := index.Cursor().Seek(prefix)
			indexed = k != nil && bytes.Compare(k, end) < 0
		}
		if !complete && cursor == nil && stateData == nil && !indexed {
			continue
		}
		if !complete && cursor == nil {
			allValid = false
			if observed.owned {
				continue
			}
			if progress != nil {
				return false, fmt.Errorf("%w: client index completion evidence missing", ErrUsageHistoryIncomplete)
			}
		}
		if cursor != nil && observed.coveredCount == 0 && stateData == nil && !indexed && !observed.coverageInvalid {
			continue
		}
		valid := false
		if hours != nil && index != nil && stateData != nil && tx.Bucket([]byte(BucketUsageEvents)) != nil {
			state, err := loadClientHour(tx, []byte(key))
			count, digest := observed.count, observed.digest
			if cursor != nil {
				count, digest = observed.coveredCount, observed.coveredDigest
			}
			if err == nil && !observed.coverageInvalid && state.Count == count && state.Digest == digest {
				_, err = readClientHourInTx(ctx, tx, observed.client, observed.hour, nil, nil)
				valid = err == nil
			}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !valid {
			allValid = false
			if cursor != nil || !observed.owned {
				return false, fmt.Errorf("%w: client/hour evidence disagrees with raw history", ErrUsageHistoryIncomplete)
			}
		}
	}
	return allValid, nil
}

// ReadClientEvents returns all retained raw events for one normalized client in
// [from, to), ordered by timestamp then original raw key. Unlike List, it has no
// display limit; batchSize bounds each View, not the returned slice. The caller
// owns the returned data and can resolve prices after all transactions close.
//
// Each intersecting UTC hour is read in bounded batches and revision-checked.
// A changing hour is attempted at most three times; an error returns no partial
// results. Hours may represent different snapshots. This is NOT proof that
// pruned history matches permanent rollups: accounting consumers must still
// check raw/rollup parity, as SumTargetModelCostsRange does today.
func (s *UsageStore) ReadClientEvents(ctx context.Context, client string, from, to time.Time, batchSize int) ([]usage.Event, error) {
	return s.readClientEvents(ctx, client, from, to, batchSize, clientIndexReadHooks{})
}

// Hooks are call-local and test-only, never mutable state on UsageStore. They
// make decode counts and inter-batch writes deterministic without timing races.
type clientIndexReadHooks struct {
	decoded    func()
	afterBatch func()
	// completeHour runs only after the retained-history digest and revision
	// match, in the same snapshot as the permanent rollups. Accounting readers
	// use it to prove parity without mixing a later rollup with earlier events.
	completeHour  func(*bolt.Tx, time.Time, []usage.Event) error
	discardEvents bool // accounting consumes completed hours through the callback
}

func (s *UsageStore) readClientEvents(ctx context.Context, client string, from, to time.Time, batchSize int, hooks clientIndexReadHooks) ([]usage.Event, error) {
	from, to = from.UTC(), to.UTC()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.db.View(requireClientIndex); err != nil {
		return nil, err
	}
	if !from.Before(to) {
		return nil, nil
	}
	var result []usage.Event
	for hour := from.Truncate(time.Hour); hour.Before(to); hour = hour.Add(time.Hour) {
		var events []usage.Event
		var err error
		for attempt := 0; attempt < clientIndexRetries; attempt++ {
			events, err = s.readClientHour(ctx, strings.TrimSpace(client), hour, from, to, clientIndexBatchSize(batchSize), hooks)
			if !errors.Is(err, ErrUsageClientIndexChanged) {
				break
			}
		}
		if err != nil {
			return nil, err
		}
		if !hooks.discardEvents {
			result = append(result, events...)
		}
	}
	return result, nil
}

func (s *UsageStore) readClientHour(ctx context.Context, client string, hour, from, to time.Time, size int, hooks clientIndexReadHooks) ([]usage.Event, error) {
	var initial usageClientHour
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := requireClientIndex(tx); err != nil {
			return err
		}
		var err error
		initial, err = loadClientHour(tx, clientHourKey(client, hour))
		return err
	})
	if err != nil {
		return nil, err
	}
	start := []byte(clientIndexPrefix(client) + hour.Format(clientIndexTimeLayout))
	end := []byte(clientIndexPrefix(client) + hour.Add(time.Hour).Format(clientIndexTimeLayout))
	var last []byte
	var count uint64
	var digest [sha256.Size]byte
	var result []usage.Event
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		exhausted := false
		err := s.db.View(func(tx *bolt.Tx) error {
			if err := requireClientIndex(tx); err != nil {
				return err
			}
			state, err := loadClientHour(tx, clientHourKey(client, hour))
			if err != nil {
				return err
			}
			if state != initial {
				return ErrUsageClientIndexChanged
			}
			cursor := tx.Bucket([]byte(BucketUsageEventsByClient)).Cursor()
			k, v := cursor.Seek(start)
			if last != nil {
				k, v = cursor.Seek(last)
				if bytes.Equal(k, last) {
					k, v = cursor.Next()
				}
			}
			n := 0
			for ; k != nil && bytes.Compare(k, end) < 0 && n < size; k, v = cursor.Next() {
				data := tx.Bucket([]byte(BucketUsageEvents)).Get(v)
				var event usage.Event
				if hooks.decoded != nil {
					hooks.decoded()
				}
				if data == nil || json.Unmarshal(data, &event) != nil || !bytes.Equal(k, clientEventIndexKey(event, v)) {
					return ErrUsageClientIndexIncomplete
				}
				count++
				xorDigest(&digest, rawDigest(v, data))
				if !event.Timestamp.Before(from) && event.Timestamp.Before(to) {
					result = append(result, event)
				}
				last = append(last[:0], k...)
				n++
			}
			exhausted = k == nil || bytes.Compare(k, end) >= 0
			return nil
		})
		if err != nil {
			return nil, err
		}
		if hooks.afterBatch != nil {
			hooks.afterBatch()
		}
		if exhausted {
			break
		}
	}
	// Verify even an empty hour and the final batch, after its View closed.
	err = s.db.View(func(tx *bolt.Tx) error {
		if err := requireClientIndex(tx); err != nil {
			return err
		}
		state, err := loadClientHour(tx, clientHourKey(client, hour))
		if err != nil {
			return err
		}
		if state != initial {
			return ErrUsageClientIndexChanged
		}
		if count != state.Count || digest != state.Digest {
			return ErrUsageClientIndexIncomplete
		}
		if hooks.completeHour != nil {
			if err := hooks.completeHour(tx, hour, result); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
